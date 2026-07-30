// Command verify-looperd-dashboard starts a built looperd artifact and proves
// that its embedded SPA and local API are usable together.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

var scriptSourcePattern = regexp.MustCompile(`<script[^>]+src="([^"]+\.js)"`)

func main() {
	binary := flag.String("binary", "", "path to a host-runnable looperd binary")
	flag.Parse()
	if strings.TrimSpace(*binary) == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/verify-looperd-dashboard --binary <path>")
		os.Exit(2)
	}
	if err := verify(*binary); err != nil {
		fmt.Fprintf(os.Stderr, "verify embedded dashboard: %v\n", err)
		os.Exit(1)
	}
}

func verify(binary string) error {
	absBinary, err := filepath.Abs(binary)
	if err != nil {
		return err
	}
	info, err := os.Stat(absBinary)
	if err != nil {
		return fmt.Errorf("stat binary %s: %w", absBinary, err)
	}
	if info.IsDir() {
		return fmt.Errorf("binary %s is a directory", absBinary)
	}

	stateDir, err := os.MkdirTemp("", "looper-dashboard-binary-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)
	port, err := availablePort()
	if err != nil {
		return err
	}
	logPath := filepath.Join(stateDir, "looperd.log")
	configPath := filepath.Join(stateDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("# isolated dashboard binary verification\n"), 0o600); err != nil {
		return err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(absBinary,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--db-path", filepath.Join(stateDir, "looper.sqlite"),
		"--log-dir", filepath.Join(stateDir, "logs"),
	)
	cmd.Env = isolatedEnvironment(stateDir, configPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	processDone := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(processDone)
	}()
	defer stopProcess(cmd, processDone)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	statusBody, err := waitForStatus(client, baseURL+"/api/v1/status", processDone, func() error { return processErr }, 15*time.Second)
	if err != nil {
		return errors.Join(err, daemonLog(logPath))
	}
	if err := requireOKEnvelope("/api/v1/status", statusBody); err != nil {
		return err
	}
	for _, apiPath := range []string{"/api/v1/healthz", "/api/v1/runs/active", "/api/v1/projects"} {
		body, _, err := get(client, baseURL+apiPath)
		if err != nil {
			return err
		}
		if err := requireOKEnvelope(apiPath, body); err != nil {
			return err
		}
	}

	index, indexHeader, err := get(client, baseURL+"/dashboard/")
	if err != nil {
		return err
	}
	if !strings.Contains(indexHeader.Get("Content-Type"), "text/html") {
		return fmt.Errorf("dashboard content type = %q, want text/html", indexHeader.Get("Content-Type"))
	}
	scriptSource, err := productionScriptSource(index)
	if err != nil {
		return err
	}
	indexURL, _ := url.Parse(baseURL + "/dashboard/")
	assetRef, err := url.Parse(scriptSource)
	if err != nil {
		return err
	}
	assetURL := indexURL.ResolveReference(assetRef).String()
	asset, assetHeader, err := get(client, assetURL)
	if err != nil {
		return err
	}
	if len(asset) < 1024 {
		return fmt.Errorf("dashboard JavaScript bundle is only %d bytes", len(asset))
	}
	if !strings.Contains(assetHeader.Get("Content-Type"), "javascript") {
		return fmt.Errorf("dashboard JavaScript content type = %q", assetHeader.Get("Content-Type"))
	}

	fmt.Printf("verified looperd dashboard: index + %s + shared local APIs\n", assetRef.Path)
	return nil
}

func requireOKEnvelope(name string, body []byte) error {
	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("%s response is not JSON: %w", name, err)
	}
	if !envelope.OK {
		return fmt.Errorf("%s response is not an ok envelope", name)
	}
	return nil
}

func productionScriptSource(index []byte) (string, error) {
	if bytes.Contains(index, []byte("Production dashboard assets are not embedded")) {
		return "", errors.New("binary served the fallback dashboard placeholder")
	}
	match := scriptSourcePattern.FindSubmatch(index)
	if len(match) != 2 {
		return "", errors.New("dashboard index contains no production JavaScript bundle")
	}
	return string(match[1]), nil
}

func isolatedEnvironment(stateDir, configPath string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "LOOPER_") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "LOOPER_HOME="+stateDir, "LOOPER_CONFIG="+configPath)
}

func availablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForStatus(client *http.Client, endpoint string, processDone <-chan struct{}, processError func() error, timeout time.Duration) ([]byte, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		body, _, err := get(client, endpoint)
		if err == nil {
			return body, nil
		}
		select {
		case <-processDone:
			return nil, fmt.Errorf("looperd exited before serving status: %v", processError())
		case <-deadline.C:
			return nil, fmt.Errorf("looperd did not serve status within %s", timeout)
		case <-ticker.C:
		}
	}
}

func get(client *http.Client, endpoint string) ([]byte, http.Header, error) {
	response, err := client.Get(endpoint)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil {
		return nil, response.Header, readErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.Header, fmt.Errorf("GET %s returned %s", endpoint, response.Status)
	}
	if len(body) > maxResponseBytes {
		return nil, response.Header, fmt.Errorf("GET %s exceeded %d bytes", endpoint, maxResponseBytes)
	}
	return body, response.Header, nil
}

func stopProcess(cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-done:
		return
	default:
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func daemonLog(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return fmt.Errorf("looperd log:\n%s", strings.TrimSpace(string(raw)))
}
