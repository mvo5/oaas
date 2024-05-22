package main

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/exp/slices"

	"github.com/sirupsen/logrus"
)

var (
	supportedBuildContentTypes = []string{"application/x-tar"}
	osbuildBinary              = "osbuild"
)

var (
	ErrAlreadyBuilding = errors.New("build already starte")
)

type writeFlusher interface {
	io.Writer
	http.Flusher
}

func followLineOutput(wg *sync.WaitGroup, r io.Reader, w writeFlusher, logf io.Writer) {
	defer wg.Done()

	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		// ReadString can return both an error and a valid line :/
		if len(line) > 0 {
			// stream output
			w.Write([]byte(line))
			w.Flush()
			// also write to the log file
			logf.Write([]byte(line))
		}
		if err != nil {
			return
		}
	}
}

func runOsbuild(buildDir string, control *controlJSON, output io.Writer) (string, error) {
	flusher, ok := output.(writeFlusher)
	if !ok {
		return "", fmt.Errorf("cannot stream the output")
	}

	logf, err := os.Create(filepath.Join(buildDir, "build.log"))
	if err != nil {
		return "", fmt.Errorf("cannot create log file: %v", err)
	}
	defer logf.Close()
	flusher.Write([]byte(fmt.Sprintf("starting %s build\n", buildDir)))
	flusher.Flush()

	outputDir := filepath.Join(buildDir, "output")
	storeDir := filepath.Join(buildDir, "store")
	cmd := exec.Command(osbuildBinary)
	for _, exp := range control.Exports {
		cmd.Args = append(cmd.Args, []string{"--export", exp}...)
	}
	cmd.Env = append(cmd.Env, control.Environments...)
	cmd.Args = append(cmd.Args, []string{"--output-dir", outputDir}...)
	cmd.Args = append(cmd.Args, []string{"--store", storeDir}...)
	cmd.Args = append(cmd.Args, filepath.Join(buildDir, "manifest.json"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	// ensure all output is flushed before exiting
	// TODO: test this
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { followLineOutput(&wg, stdout, flusher, logf) }()
	wg.Add(1)
	go func() { followLineOutput(&wg, stderr, flusher, logf) }()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return "", err
	}

	cmd = exec.Command(
		"tar",
		"-Scf",
		filepath.Join(outputDir, "output.tar"),
		"output",
	)
	cmd.Dir = buildDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		logrus.Errorf("Failed creating result tarball: %v", err)
		return "", fmt.Errorf("cannot tar output directory: %v, output:\n%s", err, out)
	}
	logrus.Infof("tar output:\n%s", out)
	return outputDir, nil
}

type controlJSON struct {
	Environments []string `json:"environments"`
	Exports      []string `json:"exports"`
}

func mustRead(atar *tar.Reader, name string) error {
	hdr, err := atar.Next()
	if err != nil {
		return fmt.Errorf("cannot read tar %v: %v", name, err)
	}
	if hdr.Name != name {
		return fmt.Errorf("expected tar %v, got %v", name, hdr.Name)
	}
	return nil
}

func handleControlJSON(atar *tar.Reader) (*controlJSON, error) {
	if err := mustRead(atar, "control.json"); err != nil {
		return nil, err
	}

	var control controlJSON
	if err := json.NewDecoder(atar).Decode(&control); err != nil {
		return nil, err
	}
	return &control, nil
}

func createBuildDir(config *Config) (string, error) {
	buildDirBase := config.BuildDirBase

	// we could create a per-build dir here but the goal is to
	// only have a single build only so we don't bother
	if err := os.MkdirAll(buildDirBase, 0700); err != nil {
		return "", fmt.Errorf("cannot create build base dir: %v", err)
	}

	// ensure there is only a single build
	buildDir := filepath.Join(buildDirBase, "build")
	if err := os.Mkdir(buildDir, 0700); err != nil {
		if os.IsExist(err) {
			return "", ErrAlreadyBuilding
		}
		return "", err
	}

	return buildDir, nil
}

func handleManifestJSON(atar *tar.Reader, buildDir string) error {
	if err := mustRead(atar, "manifest.json"); err != nil {
		return err
	}
	manifestJSONPath := filepath.Join(buildDir, "manifest.json")

	f, err := os.Create(manifestJSONPath)
	if err != nil {
		return fmt.Errorf("cannot create manifest.json: %v", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, atar); err != nil {
		return fmt.Errorf("cannot read body: %v", err)
	}

	if err := f.Close(); err != nil {
		return err
	}

	return nil
}

func bufZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func doHandleSparse(dst io.WriteSeeker, src io.Reader, buf []byte, nr int64, written *int64) (bool, error) {
	if !bufZero(buf) {
		return false, nil
	}

	if _, err := dst.Seek(int64(nr), os.SEEK_CUR); err != nil {
		return false, err
	}
	*written = nr

	return true, nil
}

func copyWithSparse(w io.Writer, src io.Reader) (written int64, err error) {
	dst, ok := w.(io.WriteSeeker)
	if !ok {
		return 0, fmt.Errorf("cannot use copyWithFile wihtout a writeSeeker, got %T", w)
	}

	// use a small buf here because our algorithm is very naive and
	// we only skip over holes at least the size of the buffer
	buf := make([]byte, 1*1024)
	// copied from golang:io.copyBuffer()
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			// this is the only addition to
			// io.copyBuffer(), everything else in this
			// loop is identical
			handled, err := doHandleSparse(dst, src, buf, int64(nr), &written)
			if err != nil {
				return 0, fmt.Errorf("sparse: %w", err)
			}
			if handled {
				continue
			}
			// -------------------------------------------

			nw, ew := dst.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = fmt.Errorf("internal errror: invalid write result")
				}
			}
			written += int64(nw)
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err
}

func handleIncludedSources(atar *tar.Reader, buildDir string) error {
	for {
		hdr, err := atar.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot read from tar %v", err)
		}

		// ensure we only allow "store/" things
		if filepath.Clean(hdr.Name) != strings.TrimSuffix(hdr.Name, "/") {
			return fmt.Errorf("name not clean: %v != %v", filepath.Clean(hdr.Name), hdr.Name)
		}
		if !strings.HasPrefix(hdr.Name, "store/") {
			return fmt.Errorf("expected store/ prefix, got %v", hdr.Name)
		}

		// this assume "well" behaving tars, i.e. all dirs that lead
		// up to the tar are included etc
		target := filepath.Join(buildDir, hdr.Name)
		mode := os.FileMode(hdr.Mode)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, mode); err != nil {
				return fmt.Errorf("unpack: %w", err)
			}
		case tar.TypeReg, tar.TypeGNUSparse:
			f, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE, mode)
			if err != nil {
				return fmt.Errorf("unpack: %w", err)
			}
			defer f.Close()

			if hdr.Typeflag == tar.TypeGNUSparse {
				if err := f.Truncate(hdr.Size); err != nil {
					return fmt.Errorf("truncate: %w", err)
				}
				if _, err := copyWithSparse(f, atar); err != nil {
					return fmt.Errorf("unpack sparse: %w", err)
				}
			} else {
				if _, err := io.Copy(f, atar); err != nil {
					return fmt.Errorf("unpack: %w", err)
				}
			}

			if err := f.Close(); err != nil {
				return fmt.Errorf("unpack: %w", err)
			}
		default:
			return fmt.Errorf("unsupported tar type %v", hdr.Typeflag)
		}
		if err := os.Chtimes(target, hdr.AccessTime, hdr.ModTime); err != nil {
			return fmt.Errorf("unpack: %w", err)
		}
	}
}

// test for real via:
// curl -o - --data-binary "@./test.tar" -H "Content-Type: application/x-tar"  -X POST http://localhost:8001/api/v1/build
func handleBuild(logger *logrus.Logger, config *Config) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			logger.Debugf("handlerBuild called on %s", r.URL.Path)
			defer r.Body.Close()

			if r.Method != http.MethodPost {
				http.Error(w, "build endpoint only supports POST", http.StatusMethodNotAllowed)
				return
			}

			contentType := r.Header.Get("Content-Type")
			if !slices.Contains(supportedBuildContentTypes, contentType) {
				http.Error(w, fmt.Sprintf("Content-Type must be %v, got %v", supportedBuildContentTypes, contentType), http.StatusUnsupportedMediaType)
				return
			}

			// control.json passes the build parameters
			atar := tar.NewReader(r.Body)
			control, err := handleControlJSON(atar)
			if err != nil {
				logger.Error(err)
				http.Error(w, "cannot decode control.json", http.StatusBadRequest)
				return
			}

			buildDir, err := createBuildDir(config)
			if err != nil {
				logger.Error(err)
				if err == ErrAlreadyBuilding {
					http.Error(w, "build already started", http.StatusConflict)
				} else {
					http.Error(w, "create build dir", http.StatusBadRequest)
				}
				return
			}

			// manifest.json is the osbuild input
			if err := handleManifestJSON(atar, buildDir); err != nil {
				logger.Error(err)
				http.Error(w, "manifest.json", http.StatusBadRequest)
				return
			}
			// extract ".osbuild/sources" here too from the tar
			if err := handleIncludedSources(atar, buildDir); err != nil {
				logger.Error(err)
				http.Error(w, "included sources/", http.StatusBadRequest)
				return
			}

			w.WriteHeader(http.StatusCreated)

			// run osbuild and stream the output to the client
			buildResult := newBuildResult(config)
			_, err = runOsbuild(buildDir, control, w)
			if werr := buildResult.Mark(err); werr != nil {
				logger.Errorf("cannot write result file %v", werr)
			}
			if err != nil {
				logger.Errorf("canot run osbuild: %v", err)
				http.Error(w, "cannot run osbuild", http.StatusInternalServerError)
				return
			}
		},
	)
}
