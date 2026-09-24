package config

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// EnvReloadAddress is the listen address of the URL-provider reload endpoint.
	EnvReloadAddress = "CONFIG_RELOAD_ADDRESS"
	// EnvReloadToken, when set, is required as "Authorization: Bearer <token>" on reload calls.
	EnvReloadToken = "CONFIG_RELOAD_TOKEN"
)

// ReloadFunc applies a freshly parsed config. It must only publish the config once it
// has been fully compiled; returning an error keeps the previous config active.
type ReloadFunc func(*Config) error

// WatchConfig starts watching the configured provider and calls onReload for every
// new valid config. Invalid configs are reported through onError and ignored.
func WatchConfig(configPath string, onReload ReloadFunc, onError func(error)) {
	if onError == nil {
		onError = func(error) {}
	}
	provider := os.Getenv(EnvConfigProvider)
	if provider == "" {
		provider = "FILE"
	}

	switch provider {
	case "K8S":
		go watchK8sConfigMap(onReload, onError)
	case "URL":
		go watchURLConfig(onReload, onError)
	default:
		go watchFileConfig(configPath, onReload, onError)
	}
}

func applyBytes(data []byte, onReload ReloadFunc) error {
	newConfig, err := ParseBytes(data)
	if err != nil {
		return err
	}
	return onReload(newConfig)
}

func watchK8sConfigMap(onReload ReloadFunc, onError func(error)) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		onError(fmt.Errorf("config watcher: in-cluster config: %w", err))
		return
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		onError(fmt.Errorf("config watcher: kubernetes client: %w", err))
		return
	}

	cmName := os.Getenv("CONFIG_K8S_NAME")
	if cmName == "" {
		cmName = "hyper-engine-config"
	}

	namespace := os.Getenv("CONFIG_K8S_NAMESPACE")
	if namespace == "" {
		namespace = "hyper-system"
	}

	lastApplied := initialK8sResourceVersion
	apply := func(cm *corev1.ConfigMap) {
		if cm.ResourceVersion == lastApplied {
			return
		}
		yamlContent, ok := cm.Data["config.yaml"]
		if !ok {
			onError(fmt.Errorf("config watcher: config.yaml missing from configmap %s/%s", namespace, cmName))
			return
		}
		if err := applyBytes([]byte(yamlContent), onReload); err != nil {
			onError(fmt.Errorf("config watcher: rejected configmap version %s: %w", cm.ResourceVersion, err))
		}
		// Record the version even when rejected so a bad version is not retried in a loop.
		lastApplied = cm.ResourceVersion
	}

	for {
		// Re-sync on every (re)connect so updates made while the watch was down are not lost.
		cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), cmName, metav1.GetOptions{})
		if err != nil {
			onError(fmt.Errorf("config watcher: get configmap: %w", err))
			time.Sleep(5 * time.Second)
			continue
		}
		apply(cm)

		watcher, err := clientset.CoreV1().ConfigMaps(namespace).Watch(context.Background(), metav1.ListOptions{
			FieldSelector:   fmt.Sprintf("metadata.name=%s", cmName),
			ResourceVersion: cm.ResourceVersion,
		})
		if err != nil {
			onError(fmt.Errorf("config watcher: watch configmap: %w", err))
			time.Sleep(5 * time.Second)
			continue
		}

		for event := range watcher.ResultChan() {
			if event.Type != watch.Added && event.Type != watch.Modified {
				continue
			}
			if cm, ok := event.Object.(*corev1.ConfigMap); ok {
				apply(cm)
			}
		}

		time.Sleep(1 * time.Second)
	}
}

func watchURLConfig(onReload ReloadFunc, onError func(error)) {
	configURL := os.Getenv(EnvConfigURL)
	if configURL == "" {
		return
	}

	token := os.Getenv(EnvReloadToken)
	addr := os.Getenv(EnvReloadAddress)
	if addr == "" {
		// Without a token the endpoint is only reachable from inside the pod.
		addr = "127.0.0.1:9002"
		if token != "" {
			addr = ":9002"
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token != "" {
			want := "Bearer " + token
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(configURL)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to fetch config: %v", err), http.StatusBadGateway)
			return
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("Remote server returned status: %d", resp.StatusCode), http.StatusBadGateway)
			return
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read config body: %v", err), http.StatusBadGateway)
			return
		}

		if err := applyBytes(data, onReload); err != nil {
			onError(fmt.Errorf("config watcher: rejected remote config: %w", err))
			http.Error(w, fmt.Sprintf("Config rejected, previous config still active: %v", err), http.StatusUnprocessableEntity)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Config successfully reloaded"))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		onError(fmt.Errorf("config watcher: reload endpoint on %s: %w", addr, err))
	}
}

func watchFileConfig(configPath string, onReload ReloadFunc, onError func(error)) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		onError(fmt.Errorf("config watcher: fsnotify: %w", err))
		return
	}
	defer func() {
		_ = watcher.Close()
	}()

	dir := filepath.Dir(configPath)
	if err := watcher.Add(dir); err != nil {
		onError(fmt.Errorf("config watcher: watch %s: %w", dir, err))
		return
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
				if filepath.Base(event.Name) == filepath.Base(configPath) || filepath.Base(event.Name) == "..data" {
					time.Sleep(100 * time.Millisecond)

					data, err := os.ReadFile(configPath)
					if err != nil {
						continue
					}

					if err := applyBytes(data, onReload); err != nil {
						onError(fmt.Errorf("config watcher: rejected %s: %w", configPath, err))
					}
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			onError(fmt.Errorf("config watcher: fsnotify: %w", err))
		}
	}
}
