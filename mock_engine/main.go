package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type authServer struct {
	authv3.UnimplementedAuthorizationServer
}

func (s *authServer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	return &authv3.CheckResponse{
		Status: status.New(codes.OK, "OK").Proto(),
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{},
		},
	}, nil
}

func watchConfigMap() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Println("Not running in-cluster, skipping ConfigMap print loop.")
		return
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf("Failed to create K8s client: %v", err)
		return
	}

	configMapName := "hyper-engine-config"
	namespace := "hyper-system"

	for {
		cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
		if err != nil {
			log.Printf("Waiting for ConfigMap '%s' to be created by Operator...", configMapName)
		} else if cm.Data != nil {
			if yamlContent, ok := cm.Data["config.yaml"]; ok {
				fmt.Println("\n==================================================")
				fmt.Println(" 📡 LIVE CONFIGMAP RECEIVED FROM OPERATOR (IN-RAM):")
				fmt.Println("==================================================")
				fmt.Println(yamlContent)
				fmt.Println("==================================================\n")
			}
		}
		time.Sleep(10 * time.Second)
	}
}

func main() {
	go watchConfigMap()

	listener, err := net.Listen("tcp", ":9001")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	authv3.RegisterAuthorizationServer(grpcServer, &authServer{})

	log.Println("Starting Go Mock ext_authz Server on port :9001...")
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
