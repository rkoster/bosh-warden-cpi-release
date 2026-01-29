package vm

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path/filepath"

	wrdn "code.cloudfoundry.org/garden"
	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
)

type wardenFileService struct {
	container wrdn.Container

	logTag string
	logger boshlog.Logger
}

func NewWardenFileService(container wrdn.Container, logger boshlog.Logger) WardenFileService {
	return &wardenFileService{
		container: container,

		logTag: "vm.wardenFileService",
		logger: logger,
	}
}

func (s *wardenFileService) Download(sourcePath string) ([]byte, error) {
	sourceFileName := filepath.Base(sourcePath)
	tmpFilePath := filepath.Join("/tmp", sourceFileName)

	s.logger.Debug(s.logTag, "Downloading file at %s", sourcePath)

	// Copy settings file to a temporary directory
	// so that tar (running as vcap) has permission to readdir.
	// (/var/vcap/bosh is owned by root.)
	script := fmt.Sprintf(
		"cp %s %s && chown vcap:vcap %s",
		sourcePath,
		tmpFilePath,
		tmpFilePath,
	)

	err := s.runPrivilegedScript(script)
	if err != nil {
		return []byte{}, bosherr.WrapError(err, "Running copy source file script")
	}

	spec := wrdn.StreamOutSpec{
		Path: tmpFilePath,
		User: "root",
	}

	streamOut, err := s.container.StreamOut(spec)
	if err != nil {
		return []byte{}, bosherr.WrapErrorf(err, "Streaming out file '%s'", sourceFileName)
	}

	tarReader := tar.NewReader(streamOut)

	_, err = tarReader.Next()
	if err != nil {
		return []byte{}, bosherr.WrapErrorf(err, "Reading tar header for '%s'", sourceFileName)
	}

	return io.ReadAll(tarReader)
}

func (s *wardenFileService) Upload(destinationPath string, contents []byte) error {
	s.logger.Debug(s.logTag, "Uploading file to %s", destinationPath)

	destinationFileName := filepath.Base(destinationPath)

	// Stream in settings file to a temporary directory
	// so that tar (running as vcap) has permission to unpack into dir.
	tarReader, err := s.tarReader(destinationFileName, contents)
	if err != nil {
		return bosherr.WrapError(err, "Creating tar")
	}

	spec := wrdn.StreamInSpec{
		Path:      "/tmp/",
		User:      "root",
		TarStream: tarReader,
	}

	err = s.container.StreamIn(spec)
	if err != nil {
		return bosherr.WrapError(err, "Streaming in tar")
	}

	tmpFilePath := filepath.Join("/tmp", destinationFileName)

	// Debug: Check /tmp before and after StreamIn to understand what's happening
	// Workaround for overlayfs race condition on Ubuntu Noble with cgroup v2.
	// StreamIn may report success before the file is visible in /tmp due to
	// filesystem sync issues with overlayfs upper layer. We sync and retry
	// the move operation to ensure the file is visible.
	script := fmt.Sprintf(
		"sync; for i in $(seq 1 20); do [ -f %s ] && mv %s %s && exit 0; sleep 0.2; done; echo 'File not found after 20 retries (4 seconds)'; echo 'Looking for file: %s'; ls -la /tmp/; find /tmp -name '%s' 2>/dev/null || true; exit 1",
		tmpFilePath,
		tmpFilePath,
		destinationPath,
		tmpFilePath,
		destinationFileName,
	)

	err = s.runPrivilegedScript(script)
	if err != nil {
		return bosherr.WrapErrorf(err, "Moving temporary file to destination '%s'", destinationPath)
	}

	return nil
}

func (s *wardenFileService) runPrivilegedScript(script string) error {
	processSpec := wrdn.ProcessSpec{
		Path: "bash",
		Args: []string{"-c", script},
		User: "root",
	}

	// Collect output for debugging
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	processIO := wrdn.ProcessIO{Stdout: stdout, Stderr: stderr}

	process, err := s.container.Run(processSpec, processIO)
	if err != nil {
		return bosherr.WrapError(err, "Running script")
	}

	exitCode, err := process.Wait()
	if err != nil {
		return bosherr.WrapError(err, "Waiting for script")
	}

	if exitCode != 0 {
		return bosherr.Errorf("Script exited with non-0 exit code, stdout: '%s' stderr: '%s'", stdout.String(), stderr.String())
	}

	return nil
}

func (s *wardenFileService) tarReader(fileName string, contents []byte) (io.Reader, error) {
	tarBytes := &bytes.Buffer{}

	tarWriter := tar.NewWriter(tarBytes)

	fileHeader := &tar.Header{
		Name: fileName,
		Size: int64(len(contents)),
		Mode: 0640,
	}

	err := tarWriter.WriteHeader(fileHeader)
	if err != nil {
		return nil, bosherr.WrapError(err, "Writing tar header")
	}

	_, err = tarWriter.Write(contents)
	if err != nil {
		return nil, bosherr.WrapError(err, "Writing file to tar")
	}

	err = tarWriter.Close()
	if err != nil {
		return nil, bosherr.WrapError(err, "Closing tar writer")
	}

	return tarBytes, nil
}
