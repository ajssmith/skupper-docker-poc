all: build

build:
	go build -o skupper-docker cmd/skupper-docker/main.go

clean:
	rm -rf skupper-docker release

deps:
	dep ensure

