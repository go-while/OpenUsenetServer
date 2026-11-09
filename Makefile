.PHONY: build test e2e vet run compose-up compose-down

BIN := bin/openusenet

build:
	mkdir -p bin
	go build -o $(BIN) ./cmd/openusenet

vet:
	go vet ./...

test:
	go test ./...

e2e:
	cd e2e && go test -tags e2e -count=1 -v .

run: build
	./$(BIN) serve --config config.example.yml

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down
