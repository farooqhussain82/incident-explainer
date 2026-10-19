IMAGE   ?= incident-explainer:dev
CLUSTER ?= incident-demo
ARCH    ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

.PHONY: test build image kind-up kind-load deploy demo ui clean

test:
	go vet ./...
	go test ./...

build:
	go build -o bin/incident-explainer .

# Static Linux binary + CA bundle, then a FROM scratch image.
image:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o dist/incident-explainer .
	cp /etc/ssl/cert.pem dist/ca-certificates.crt 2>/dev/null || cp /etc/ssl/certs/ca-certificates.crt dist/ca-certificates.crt
	docker build -t $(IMAGE) .

kind-up:
	kind create cluster --name $(CLUSTER) --wait 90s

kind-load: image
	kind load docker-image $(IMAGE) --name $(CLUSTER)

deploy: kind-load
	kubectl --context kind-$(CLUSTER) apply -f deploy/incident-explainer.yaml
	kubectl --context kind-$(CLUSTER) -n incident-explainer rollout status deploy/incident-explainer

demo:
	kubectl --context kind-$(CLUSTER) apply -f demo/broken-pods.yaml

ui:
	kubectl --context kind-$(CLUSTER) -n incident-explainer port-forward svc/incident-explainer 8080:80

clean:
	rm -rf bin dist
