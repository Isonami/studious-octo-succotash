package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// launchSyncWorker starts a worker independently of the server's lifetime.
// Kubernetes workers need shared persistent storage for DataPath and any file
// configuration: cloning volumes does not make emptyDir or container files shared.
// The service account needs permission to get its pod and create namespace Jobs.
func launchSyncWorker(ctx context.Context, config Config, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, inCluster := os.LookupEnv("KUBERNETES_SERVICE_HOST"); inCluster {
		return launchInClusterSyncWorker(ctx, config, id, "/var/run/secrets/kubernetes.io/serviceaccount")
	}
	cmd, err := detachedSyncWorkerCommand(config, id, os.Args[1:])
	if err != nil {
		return err
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open worker output: %w", err)
	}
	defer null.Close()
	cmd.Stdout, cmd.Stderr = null, null
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sync worker: %w", err)
	}
	// Do not use CommandContext: cancellation of the request that launched the
	// worker must not kill it. Wait still reaps the child while the server lives.
	go func() { _ = cmd.Wait() }()
	return nil
}

func syncWorkerExecutable(config Config) (string, error) {
	if config.WorkerExecutable != "" {
		return config.WorkerExecutable, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate worker executable: %w", err)
	}
	return executable, nil
}

func syncWorkerArgs(id string, serverArgs []string) []string {
	// Confita's flags backend uses the config tag verbatim (sync_worker), not
	// a kebab-case translation. Put the flag first, before any positional args.
	args := []string{"--sync_worker=" + id}
	for i := 0; i < len(serverArgs); i++ {
		arg := serverArgs[i]
		if arg == "--" {
			return append(args, serverArgs[i:]...)
		}
		name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		key, _, hasValue := strings.Cut(name, "=")
		if strings.HasPrefix(arg, "-") && (key == "sync_worker" || key == "sync-worker") {
			if !hasValue && i+1 < len(serverArgs) {
				i++
			}
			continue
		}
		args = append(args, arg)
	}
	return args
}

func detachedSyncWorkerCommand(config Config, id string, serverArgs []string) (*exec.Cmd, error) {
	executable, err := syncWorkerExecutable(config)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, syncWorkerArgs(id, serverArgs)...)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// A nil stdin is connected to /dev/null by os/exec.
	return cmd, nil
}

func launchInClusterSyncWorker(ctx context.Context, config Config, id, serviceAccountDir string) error {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return fmt.Errorf("KUBERNETES_SERVICE_HOST is empty")
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if port == "" {
		port = "443"
	}
	token, err := os.ReadFile(filepath.Join(serviceAccountDir, "token"))
	if err != nil {
		return fmt.Errorf("read Kubernetes service account token: %w", err)
	}
	namespace, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
	if err != nil {
		return fmt.Errorf("read Kubernetes namespace: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		return fmt.Errorf("read Kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("invalid Kubernetes CA certificate")
	}
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = os.Getenv("HOSTNAME")
	}
	if strings.TrimSpace(string(token)) == "" || strings.TrimSpace(string(namespace)) == "" || podName == "" {
		return fmt.Errorf("Kubernetes token, namespace and pod name must not be empty")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		// Never forward a service account credential to a redirect target.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	baseURL := "https://" + net.JoinHostPort(host, port)
	return createSyncWorkerJob(ctx, client, baseURL, strings.TrimSpace(string(token)), strings.TrimSpace(string(namespace)), podName, config, id)
}

func createSyncWorkerJob(ctx context.Context, client *http.Client, baseURL, token, namespace, podName string, config Config, id string) error {
	var pod map[string]any
	podPath := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(podName)
	if err := syncWorkerAPIRequest(ctx, client, http.MethodGet, baseURL+podPath, token, nil, &pod); err != nil {
		return fmt.Errorf("get worker source pod: %w", err)
	}
	job, err := syncWorkerJob(pod, config, id, os.Args[1:])
	if err != nil {
		return err
	}
	jobPath := "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs"
	if err := syncWorkerAPIRequest(ctx, client, http.MethodPost, baseURL+jobPath, token, job, nil); err != nil {
		return fmt.Errorf("create sync worker Job: %w", err)
	}
	return nil
}

func syncWorkerAPIRequest(ctx context.Context, client *http.Client, method, endpoint, token string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Kubernetes request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Kubernetes API %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode Kubernetes response: %w", err)
		}
	}
	return nil
}

func syncWorkerJob(pod map[string]any, config Config, id string, serverArgs []string) (map[string]any, error) {
	// Deep copy via JSON so arbitrary Kubernetes fields survive without importing
	// Kubernetes libraries, and the source pod is never mutated.
	data, err := json.Marshal(pod["spec"])
	if err != nil {
		return nil, fmt.Errorf("copy pod spec: %w", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil || spec == nil {
		return nil, fmt.Errorf("source pod has no valid spec")
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) == 0 {
		return nil, fmt.Errorf("source pod has no containers")
	}
	var app map[string]any
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("source pod has invalid container")
		}
		if (config.KubernetesContainer == "" && len(containers) == 1) || (config.KubernetesContainer != "" && container["name"] == config.KubernetesContainer) {
			app = container
		}
	}
	if app == nil {
		return nil, fmt.Errorf("select worker container: set KubernetesContainer to an existing app container (source has %d containers)", len(containers))
	}
	executable, err := syncWorkerExecutable(config)
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"livenessProbe", "readinessProbe", "startupProbe", "lifecycle", "ports"} {
		delete(app, field)
	}
	app["command"] = []string{executable}
	app["args"] = syncWorkerArgs(id, serverArgs)
	spec["containers"] = []any{app}
	spec["restartPolicy"] = "Never"
	for _, field := range []string{"nodeName", "hostname", "subdomain", "ephemeralContainers"} {
		delete(spec, field)
	}
	// Fresh metadata deliberately omits labels, annotations and owner references:
	// server labels could make a Service route requests to this short-lived Job.
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"generateName": "sync-worker-"},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 300,
			"template": map[string]any{
				"metadata": map[string]any{},
				"spec":     spec,
			},
		},
	}, nil
}
