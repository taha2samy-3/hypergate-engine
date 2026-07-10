variable "REGISTRY" {
  default = "ghcr.io/taha2samy-2"
}

variable "TAG" {
  default = "latest"
}

group "default" {
  targets = ["engine", "operator"]
}

target "engine" {
  context = "."
  dockerfile = "Dockerfile"
  platforms = ["linux/amd64", "linux/arm64"]
  tags = ["${REGISTRY}/hyper-engine:${TAG}"]
}

target "operator" {
  context = "."
  dockerfile = "hyper-operator/Dockerfile"
  platforms = ["linux/amd64", "linux/arm64"]
  tags = ["${REGISTRY}/hyper-operator:${TAG}"]
}
