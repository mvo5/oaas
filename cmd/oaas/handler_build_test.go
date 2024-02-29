package main_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
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
	hdr := &tar.Header{Name: name, Size: int64(len(content))}
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
	return buf
}

func TestBuildIntegration(t *testing.T) {
	baseURL, baseBuildDir, _ := runTestServer(t)
	endpoint := baseURL + "api/v1/build"

	// osbuild is called with --export tree and then the manifest.json
	restore := main.MockOsbuildBinary(t, fmt.Sprintf(`#!/bin/sh
# echo our inputs for the test to validate
echo fake-osbuild "$1" "$2" "$3" "$4"
echo ---
cat "$5"

# simulate output
mkdir -p %[1]s/build/output
echo "fake-build-result" > %[1]s/build/output/disk.img
`, baseBuildDir))
	defer restore()

	buf := makeTestPost(t, `{"exports": ["tree"]}`, `{"fake": "manifest"}`)
	rsp, err := http.Post(endpoint, "application/x-tar", buf)
	assert.NoError(t, err)

	assert.Equal(t, rsp.StatusCode, http.StatusCreated)
	reader := bufio.NewReader(rsp.Body)
	line, err := reader.ReadString('\n')
	assert.NoError(t, err)
	assert.Regexp(t, fmt.Sprintf("starting %s/build build", baseBuildDir), line)

	// check that we get the output of osbuild streamed to us
	expectedContent := fmt.Sprintf(`fake-osbuild --export tree --output-dir %s/build/output
---
{"fake": "manifest"}`, baseBuildDir)
	content, err := ioutil.ReadAll(reader)
	assert.NoError(t, err)
	assert.Equal(t, string(content), expectedContent)
	// check log too
	logFileContent, err := ioutil.ReadFile(filepath.Join(baseBuildDir, "build/build.log"))
	assert.NoError(t, err)
	assert.Equal(t, string(logFileContent), expectedContent)

	// now get the result
	endpoint = baseURL + "api/v1/result/disk.img"
	rsp, err = http.Get(endpoint)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rsp.StatusCode)
	body, err := ioutil.ReadAll(rsp.Body)
	assert.NoError(t, err)
	assert.Equal(t, "fake-build-result\n", string(body))
}

func TestBuildErrorsForMultipleBuilds(t *testing.T) {
	restore := main.MockOsbuildBinary(t, `#!/bin/sh
`)
	defer restore()

	baseURL, _, loggerHook := runTestServer(t)
	endpoint := baseURL + "api/v1/build"

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
