variable "REGISTRY" {
  default = "ghcr.io/tigrisdata-community/mithras"
}

variable "TAG" {
  default = "latest"
}

group "default" {
  targets = ["webhookd"]
}

target "webhookd" {
  context    = "."
  dockerfile = "docker/webhookd.Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
  tags       = ["${REGISTRY}/webhookd:${TAG}"]
  labels = {
    "org.opencontainers.image.source"      = "https://github.com/tigrisdata-community/mithras"
    "org.opencontainers.image.title"       = "webhookd"
    "org.opencontainers.image.description" = "HTTP front-end for a mithras agent"
    "org.opencontainers.image.licenses"    = "MIT"
  }
}
