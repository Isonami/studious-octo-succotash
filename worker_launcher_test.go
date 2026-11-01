package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"flag"
	"github.com/heetch/confita"
	"github.com/heetch/confita/backend/flags"
)

func workerSourcePod(t *testing.T) map[string]any {
	t.Helper()
	var pod map[string]any
	if err := json.Unmarshal([]byte(`{
		"metadata":{"name":"server-1","uid":"old-uid","resourceVersion":"123","labels":{"app":"server"},"ownerReferences":[{"name":"deployment"}]},
		"spec":{
			"containers":[
				{"name":"sidecar","image":"proxy"},
				{"name":"app","image":"app:v1","command":["/old"],"args":["serve"],
				 "env":[{"name":"DATA_PATH","value":"/data"},{"name":"SECRET","valueFrom":{"secretKeyRef":{"name":"secret","key":"key"}}}],
				 "envFrom":[{"configMapRef":{"name":"config"}}],
				 "volumeMounts":[{"name":"data","mountPath":"/data"}],
				 "securityContext":{"runAsUser":1000},"resources":{"requests":{"cpu":"100m"}},
				 "workingDir":"/data","livenessProbe":{},"readinessProbe":{},"startupProbe":{},"lifecycle":{},"ports":[{"containerPort":8080}]}
			],
			"initContainers":[{"name":"init","image":"init:v1","args":["prepare"]}],
			"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"shared"}}],
			"securityContext":{"fsGroup":1000},"serviceAccountName":"worker-sa",
			"imagePullSecrets":[{"name":"registry"}],"nodeSelector":{"pool":"data"},
			"affinity":{"nodeAffinity":{}},"tolerations":[{"key":"data","operator":"Exists"}],
			"dnsPolicy":"ClusterFirst","priorityClassName":"batch","terminationGracePeriodSeconds":30,
			"restartPolicy":"Always","nodeName":"old-node","hostname":"server","subdomain":"servers",
			"ephemeralContainers":[{"name":"debug","image":"debug"}]
		}
	}`), &pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func TestInClusterSyncWorkerHTTPJob(t *testing.T) {
	pod := workerSourcePod(t)
	oldArgs := os.Args
	os.Args = []string{"server", "--data_path=/data", "--port", "8080", "--sync_worker=old"}
	t.Cleanup(func() { os.Args = oldArgs })
	var posted map[string]any
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer service-token" {
			t.Errorf("missing service account authorization")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/work/pods/override-pod":
			_ = json.NewEncoder(w).Encode(pod)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/batch/v1/namespaces/work/jobs":
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing JSON content type")
			}
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", port)
	t.Setenv("POD_NAME", "override-pod")
	t.Setenv("HOSTNAME", "wrong-pod")
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"token": []byte("service-token\n"), "namespace": []byte("work\n"),
		"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{KubernetesContainer: "app", WorkerExecutable: "/app/worker"}
	if err := launchInClusterSyncWorker(context.Background(), config, "sync-123", dir); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || posted == nil {
		t.Fatalf("requests = %d, job = %v", requests, posted)
	}
	if posted["apiVersion"] != "batch/v1" || posted["kind"] != "Job" {
		t.Fatalf("invalid Job: %v", posted)
	}
	if !reflect.DeepEqual(posted["metadata"], map[string]any{"generateName": "sync-worker-"}) {
		t.Fatalf("unsafe Job metadata: %v", posted["metadata"])
	}
	jobSpec := posted["spec"].(map[string]any)
	if jobSpec["backoffLimit"] != float64(0) || jobSpec["ttlSecondsAfterFinished"] != float64(300) {
		t.Fatalf("incorrect Job policies: %v", jobSpec)
	}
	template := jobSpec["template"].(map[string]any)
	if len(template["metadata"].(map[string]any)) != 0 {
		t.Fatalf("source metadata copied: %v", template["metadata"])
	}
	spec := template["spec"].(map[string]any)
	source := pod["spec"].(map[string]any)
	for _, key := range []string{"initContainers", "volumes", "securityContext", "serviceAccountName", "imagePullSecrets", "nodeSelector", "affinity", "tolerations", "dnsPolicy", "priorityClassName", "terminationGracePeriodSeconds"} {
		if !reflect.DeepEqual(spec[key], source[key]) {
			t.Errorf("pod field %s not preserved: %v", key, spec[key])
		}
	}
	if spec["restartPolicy"] != "Never" {
		t.Error("worker must never restart")
	}
	for _, key := range []string{"nodeName", "hostname", "subdomain", "ephemeralContainers"} {
		if _, exists := spec[key]; exists {
			t.Errorf("unsafe field retained: %s", key)
		}
	}
	containers := spec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("sidecar retained: %v", containers)
	}
	app := containers[0].(map[string]any)
	originalApp := source["containers"].([]any)[1].(map[string]any)
	for _, key := range []string{"name", "image", "env", "envFrom", "volumeMounts", "securityContext", "resources", "workingDir"} {
		if !reflect.DeepEqual(app[key], originalApp[key]) {
			t.Errorf("container field %s not preserved", key)
		}
	}
	for _, key := range []string{"livenessProbe", "readinessProbe", "startupProbe", "lifecycle", "ports"} {
		if _, exists := app[key]; exists {
			t.Errorf("server-only field retained: %s", key)
		}
	}
	if !reflect.DeepEqual(app["command"], []any{"/app/worker"}) || !reflect.DeepEqual(app["args"], []any{"--sync_worker=sync-123", "--data_path=/data", "--port", "8080"}) {
		t.Fatalf("worker command/args: %v %v", app["command"], app["args"])
	}
	if originalApp["livenessProbe"] == nil || source["nodeName"] != "old-node" {
		t.Error("source pod mutated")
	}
}

func TestSyncWorkerContainerSelection(t *testing.T) {
	for _, name := range []string{"", "missing"} {
		if _, err := syncWorkerJob(workerSourcePod(t), Config{KubernetesContainer: name}, "id", nil); err == nil {
			t.Errorf("container selection %q should fail", name)
		}
	}
	pod := workerSourcePod(t)
	spec := pod["spec"].(map[string]any)
	spec["containers"] = spec["containers"].([]any)[1:]
	job, err := syncWorkerJob(pod, Config{}, "id", nil)
	if err != nil {
		t.Fatal(err)
	}
	app := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(app["command"], []string{executable}) {
		t.Errorf("default executable = %v", app["command"])
	}
	for _, invalid := range []map[string]any{{}, {"spec": map[string]any{}}, {"spec": "bad"}} {
		if _, err := syncWorkerJob(invalid, Config{}, "id", nil); err == nil {
			t.Errorf("invalid pod accepted: %v", invalid)
		}
	}
}

func TestSyncWorkerAPIErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		getStatus  int
		postStatus int
		body       string
	}{
		{"get denied", 403, 201, `{"message":"forbidden"}`},
		{"post denied", 200, 403, ""},
		{"malformed pod", 200, 201, "not json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					w.WriteHeader(test.postStatus)
					_, _ = fmt.Fprint(w, "forbidden")
					return
				}
				w.WriteHeader(test.getStatus)
				if test.body != "" {
					_, _ = fmt.Fprint(w, test.body)
				} else {
					_ = json.NewEncoder(w).Encode(workerSourcePod(t))
				}
			}))
			defer server.Close()
			err := createSyncWorkerJob(context.Background(), server.Client(), server.URL, "token", "namespace", "pod", Config{KubernetesContainer: "app"}, "id")
			if err == nil {
				t.Fatal("expected Kubernetes API failure")
			}
			if test.name != "post denied" && posts != 0 {
				t.Error("attempted Job creation after GET failure")
			}
		})
	}
}

func TestDetachedSyncWorkerCommand(t *testing.T) {
	t.Setenv("WORKER_TEST_CONFIG", "inherited")
	cmd, err := detachedSyncWorkerCommand(Config{WorkerExecutable: "/bin/true"}, "new", []string{
		"--data_path=/data", "--sync_worker=old", "-sync_worker", "older", "--sync-worker=oldest", "--remote_host", "example",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/true", "--sync_worker=new", "--data_path=/data", "--remote_host", "example"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid || cmd.Stdin != nil || cmd.Cancel != nil {
		t.Fatal("command is not detached with null stdin and independent lifetime")
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "WORKER_TEST_CONFIG=inherited") {
		t.Error("configuration environment not inherited")
	}
	args := syncWorkerArgs("id", []string{"--", "--sync_worker=positional"})
	if !reflect.DeepEqual(args, []string{"--sync_worker=id", "--", "--sync_worker=positional"}) {
		t.Fatalf("positional arguments changed: %q", args)
	}
}

func TestSyncWorkerFlagConfita(t *testing.T) {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	flag.CommandLine = flag.NewFlagSet("worker-test", flag.ContinueOnError)
	os.Args = []string{"worker-test", "--sync_worker=sync-id"}
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	var config struct {
		SyncWorker string `config:"sync_worker"`
	}
	if err := confita.NewLoader(flags.NewBackend()).Load(context.Background(), &config); err != nil {
		t.Fatal(err)
	}
	if config.SyncWorker != "sync-id" {
		t.Fatalf("confita worker flag = %q", config.SyncWorker)
	}
}

func TestLaunchSyncWorkerDoesNotFallbackFromKubernetes(t *testing.T) {
	// Even an explicitly empty cluster host must fail rather than start locally.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if err := launchSyncWorker(context.Background(), Config{WorkerExecutable: "/bin/true"}, "id"); err == nil {
		t.Fatal("Kubernetes configuration error fell back to subprocess")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := launchSyncWorker(ctx, Config{}, "id"); err != context.Canceled {
		t.Fatalf("canceled launch returned %v", err)
	}
}
