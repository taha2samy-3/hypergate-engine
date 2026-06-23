package config

import (
	"context"
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

func WatchConfig(configPath string, onReload func(*Config)) {
	provider := os.Getenv(EnvConfigProvider)
	if provider == "" {
		provider = "FILE"
	}

	switch provider {
	case "K8S":
		go watchK8sConfigMap(onReload)
	case "URL":
		go watchURLConfig(onReload)
	case "FILE":
		fallthrough
	default:
		go watchFileConfig(configPath, onReload)
	}
}

func watchK8sConfigMap(onReload func(*Config)) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
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

	for {
		watcher, err := clientset.CoreV1().ConfigMaps(namespace).Watch(context.Background(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", cmName),
		})
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		for event := range watcher.ResultChan() {
			if event.Type == watch.Modified {
				cm, ok := event.Object.(*corev1.ConfigMap)
				if !ok {
					continue
				}

				if cm.Data != nil {
					if yamlContent, ok := cm.Data["config.yaml"]; ok {
						newConfig, err := ParseBytes([]byte(yamlContent))
						if err != nil {
							continue
						}

						GlobalConfig.Store(newConfig)

						if onReload != nil {
							onReload(newConfig)
						}
					}
				}
			}
		}

		time.Sleep(1 * time.Second)
	}
}

func watchURLConfig(onReload func(*Config)) {
	configURL := os.Getenv(EnvConfigURL)
	if configURL == "" {
		return
	}

	http.HandleFunc("/v1/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(configURL)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to fetch config: %v", err), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("Remote server returned status: %d", resp.StatusCode), http.StatusInternalServerError)
			return
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read config body: %v", err), http.StatusInternalServerError)
			return
		}

		newConfig, err := ParseBytes(data)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse config: %v", err), http.StatusInternalServerError)
			return
		}

		GlobalConfig.Store(newConfig)

		if onReload != nil {
			onReload(newConfig)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Config successfully reloaded"))
	})

	_ = http.ListenAndServe(":9002", nil)
}

func watchFileConfig(configPath string, onReload func(*Config)) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer watcher.Close()

	dir := filepath.Dir(configPath)
	err = watcher.Add(dir)
	if err != nil {
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

					newConfig, err := ParseBytes(data)
					if err != nil {
						continue
					}

					GlobalConfig.Store(newConfig)

					if onReload != nil {
						onReload(newConfig)
					}
				}
			}

		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		}
	}
}
