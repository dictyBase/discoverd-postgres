build:
	rm -rf bin
	mkdir bin
	godep get ./...
	go build -o bin/discoverd-postgres command.go

container:
	docker build -rm -t cybersiddhu/discoverd-postgres .
