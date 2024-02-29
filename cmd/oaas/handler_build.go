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
	"slices"
	"sync"

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
	cmd := exec.Command(osbuildBinary)
	for _, exp := range control.Exports {
		cmd.Args = append(cmd.Args, []string{"--export", exp}...)
	}
	cmd.Args = append(cmd.Args, []string{"--output-dir", outputDir}...)
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

	return outputDir, nil
}

type controlJSON struct {
	Exports []string `json:"exports"`
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
			// TODO: extract ".osbuild/sources" here too from the tar
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
