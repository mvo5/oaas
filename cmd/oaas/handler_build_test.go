package main_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	main "github.com/osbuild/oaas/cmd/oaas"
)

func TestBuildMustPOST(t *testing.T) {
	baseURL, _, loggerHook := runTestServer(t)

	endpoint := baseURL + "api/v1/build"
	rsp, err := http.Get(endpoint)
	assert.NoError(t, err)
	assert.Equal(t, rsp.StatusCode, 405)
	assert.Equal(t, loggerHook.LastEntry().Message, "handlerBuild called on /api/v1/build")
}

func writeToTar(atar *tar.Writer, name, content string) error {
	hdr := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: int64(len(content)),
	}
	if err := atar.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := atar.Write([]byte(content))
	return err
}

func TestBuildChecksContentType(t *testing.T) {
	baseURL, _, _ := runTestServer(t)

	endpoint := baseURL + "api/v1/build"
	rsp, err := http.Post(endpoint, "random/encoding", nil)
	assert.NoError(t, err)
	assert.Equal(t, rsp.StatusCode, http.StatusUnsupportedMediaType)
	body, err := ioutil.ReadAll(rsp.Body)
	assert.NoError(t, err)
	assert.Equal(t, string(body), "Content-Type must be [application/x-tar], got random/encoding\n")
}

func makeTestPost(t *testing.T, controlJSON, manifestJSON string) *bytes.Buffer {
	buf := bytes.NewBuffer(nil)
	archive := tar.NewWriter(buf)
	err := writeToTar(archive, "control.json", controlJSON)
	assert.NoError(t, err)
	err = writeToTar(archive, "manifest.json", manifestJSON)
	assert.NoError(t, err)
	// for now we assume we get validated data, for files we could
	// trivially validate on the fly but for containers that is
	// harder
	for _, dir := range []string{"store/", "store/sources", "store/sources/org.osbuild.files"} {
		err = archive.WriteHeader(&tar.Header{
			Name:     dir,
			Mode:     0755,
			Typeflag: tar.TypeDir,
		})
		assert.NoError(t, err)
	}
	err = writeToTar(archive, "store/sources/org.osbuild.files/sha256:ff800c5263b915d8a0776be5620575df2d478332ad35e8dd18def6a8c720f9c7", "random-data")
	assert.NoError(t, err)
	err = writeToTar(archive, "store/sources/org.osbuild.files/sha256:aabbcc5263b915d8a0776be5620575df2d478332ad35e8dd18def6a8c720f9c7", "other-data")
	assert.NoError(t, err)
	return buf
}

func TestBuildIntegration(t *testing.T) {
	baseURL, baseBuildDir, _ := runTestServer(t)
	endpoint := baseURL + "api/v1/build"

	// osbuild is called with --export tree and then the manifest.json
	restore := main.MockOsbuildBinary(t, fmt.Sprintf(`#!/bin/sh -e
# echo our inputs for the test to validate
echo fake-osbuild "$1" "$2" "$3" "$4" "$5" "$6"
echo ---
cat "$7"

test "$MY" = "env"

# simulate output
mkdir -p %[1]s/build/output/image
echo "fake-build-result" > %[1]s/build/output/image/disk.img
`, baseBuildDir))
	defer restore()

	buf := makeTestPost(t, `{"exports": ["tree"], "environments": ["MY=env"]}`, `{"fake": "manifest"}`)
	rsp, err := http.Post(endpoint, "application/x-tar", buf)
	assert.NoError(t, err)

	assert.Equal(t, rsp.StatusCode, http.StatusCreated)
	reader := bufio.NewReader(rsp.Body)
	line, err := reader.ReadString('\n')
	assert.NoError(t, err)
	assert.Regexp(t, fmt.Sprintf("starting %s/build build", baseBuildDir), line)

	// check that we get the output of osbuild streamed to us
	expectedContent := fmt.Sprintf(`fake-osbuild --export tree --output-dir %[1]s/build/output --store %[1]s/build/store
---
{"fake": "manifest"}`, baseBuildDir)
	content, err := ioutil.ReadAll(reader)
	assert.NoError(t, err)
	assert.Equal(t, string(content), expectedContent)
	// check log too
	logFileContent, err := ioutil.ReadFile(filepath.Join(baseBuildDir, "build/build.log"))
	assert.NoError(t, err)
	assert.Equal(t, string(logFileContent), expectedContent)
	// check that the "store" dir got created
	stat, err := os.Stat(filepath.Join(baseBuildDir, "build/store"))
	assert.NoError(t, err)
	assert.True(t, stat.IsDir())

	// now get the result
	endpoint = baseURL + "api/v1/result/image/disk.img"
	rsp, err = http.Get(endpoint)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rsp.StatusCode)
	body, err := ioutil.ReadAll(rsp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "fake-build-result\n", string(body))

	// check that the output tarball has the disk in it
	endpoint = baseURL + "api/v1/result/output.tar"
	rsp, err = http.Get(endpoint)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rsp.StatusCode)
	body, err = ioutil.ReadAll(rsp.Body)
	assert.NoError(t, err)
	tarPath := filepath.Join(baseBuildDir, "output.tar")
	assert.NoError(t, os.WriteFile(tarPath, body, 0644))
	cmd := exec.Command("tar", "-tf", tarPath)
	out, err := cmd.Output()
	assert.NoError(t, err)
	assert.Equal(t, "output/\noutput/image/\noutput/image/disk.img\n", string(out))
}

func TestBuildErrorsForMultipleBuilds(t *testing.T) {
	baseURL, buildDir, loggerHook := runTestServer(t)
	endpoint := baseURL + "api/v1/build"

	restore := main.MockOsbuildBinary(t, fmt.Sprintf(`#!/bin/sh

mkdir -p %[1]s/build/output/image
echo "fake-build-result" > %[1]s/build/output/image/disk.img
`, buildDir))
	defer restore()

	buf := makeTestPost(t, `{"exports": ["tree"]}`, `{"fake": "manifest"}`)
	rsp, err := http.Post(endpoint, "application/x-tar", buf)
	assert.NoError(t, err)
	assert.Equal(t, rsp.StatusCode, http.StatusCreated)
	defer ioutil.ReadAll(rsp.Body)

	buf = makeTestPost(t, `{"exports": ["tree"]}`, `{"fake": "manifest"}`)
	rsp, err = http.Post(endpoint, "application/x-tar", buf)
	assert.NoError(t, err)
	assert.Equal(t, rsp.StatusCode, http.StatusConflict)
	assert.Equal(t, loggerHook.LastEntry().Message, main.ErrAlreadyBuilding.Error())
}

func TestHandleIncludedSourcesUnclean(t *testing.T) {
	tmpdir := t.TempDir()

	buf := bytes.NewBuffer(nil)
	atar := tar.NewWriter(buf)
	err := writeToTar(atar, "store/../../etc/passwd", "some-content")
	assert.NoError(t, err)

	err = main.HandleIncludedSources(tar.NewReader(buf), tmpdir)
	assert.EqualError(t, err, "name not clean: ../etc/passwd != store/../../etc/passwd")
}

func TestHandleIncludedSourcesNotFromStore(t *testing.T) {
	tmpdir := t.TempDir()

	buf := bytes.NewBuffer(nil)
	atar := tar.NewWriter(buf)
	err := writeToTar(atar, "not-store", "some-content")
	assert.NoError(t, err)

	err = main.HandleIncludedSources(tar.NewReader(buf), tmpdir)
	assert.EqualError(t, err, "expected store/ prefix, got not-store")
}

func TestHandleIncludedSourcesBadTypes(t *testing.T) {
	tmpdir := t.TempDir()

	for _, badType := range []byte{tar.TypeLink, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		buf := bytes.NewBuffer(nil)
		atar := tar.NewWriter(buf)
		err := atar.WriteHeader(&tar.Header{
			Name:     "store/bad-type",
			Typeflag: badType,
		})
		assert.NoError(t, err)

		err = main.HandleIncludedSources(tar.NewReader(buf), tmpdir)
		assert.EqualError(t, err, fmt.Sprintf("unsupported tar type %v", badType))
	}
}
