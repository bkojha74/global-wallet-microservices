.PHONY: build build-linux proto up up-mongodb up-queue up-auth up-fx up-logging up-monitoring up-quality up-app down down-mongodb down-queue down-auth down-fx down-logging down-monitoring down-quality down-app logs test clean k8s-build k8s-deploy sonar-scan sast-scan

proto:
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       proto/wallet/wallet.proto \
	       proto/ledger/ledger.proto \
	       proto/auth/auth.proto \
	       proto/fx/fx.proto

build:
	go build -v -o bin/ ./cmd/...

build-linux:
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o bin/ ./cmd/...

up: up-mongodb up-queue up-auth up-fx up-logging
	docker compose -f docker-compose.yml up --build -d
	@echo "Services are spinning up. Run 'make logs' or wait 10s then 'make test'."

up-mongodb:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker volume inspect global-wallet-microservices_mongo_data >/dev/null 2>&1 || docker volume create global-wallet-microservices_mongo_data
	docker compose -f docker-compose.mongodb.yml up -d

up-queue:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker compose -f docker-compose.rabbitmq.yml up -d

up-auth:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker compose -f docker-compose.auth.yml up --build -d

up-fx:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker compose -f docker-compose.fx.yml up --build -d

up-logging:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker compose -f docker-compose.logging.yml up --build -d

up-monitoring:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker compose -f docker-compose.monitoring.yml up -d

up-quality:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker compose -f docker-compose.quality.yml up -d
	@echo "SonarQube is starting up at http://localhost:9000 (Default login: admin/admin). Please allow ~45-60s for initialization."

up-app:
	docker compose -f docker-compose.yml up --build -d

down:
	docker compose -f docker-compose.yml down
	docker compose -f docker-compose.fx.yml down
	docker compose -f docker-compose.logging.yml down
	docker compose -f docker-compose.auth.yml down
	docker compose -f docker-compose.monitoring.yml down
	docker compose -f docker-compose.rabbitmq.yml down
	docker compose -f docker-compose.mongodb.yml down

down-app:
	docker compose -f docker-compose.yml down

down-fx:
	docker compose -f docker-compose.fx.yml down

down-monitoring:
	docker compose -f docker-compose.monitoring.yml down

down-quality:
	docker compose -f docker-compose.quality.yml down

down-logging:
	docker compose -f docker-compose.logging.yml down

down-auth:
	docker compose -f docker-compose.auth.yml down

down-queue:
	docker compose -f docker-compose.rabbitmq.yml down

down-mongodb:
	docker compose -f docker-compose.mongodb.yml down

logs:
	docker compose -f docker-compose.yml logs -f

test:
	@chmod +x scripts/test-e2e.sh
	./scripts/test-e2e.sh

sonar-scan:
	@chmod +x scripts/run-sonar-scan.sh
	./scripts/run-sonar-scan.sh

sast-scan:
	@chmod +x scripts/run-sast-scan.sh
	./scripts/run-sast-scan.sh

clean:
	docker compose -f docker-compose.yml down -v --rmi all
	docker compose -f docker-compose.fx.yml down -v
	docker compose -f docker-compose.monitoring.yml down -v
	docker compose -f docker-compose.logging.yml down -v
	docker compose -f docker-compose.auth.yml down -v
	docker compose -f docker-compose.rabbitmq.yml down -v
	docker compose -f docker-compose.mongodb.yml down -v
	docker volume rm global-wallet-microservices_mongo_data

k8s-deploy:
	@chmod +x scripts/deploy-k8s.sh 2>/dev/null || true
	@./scripts/deploy-k8s.sh

k8s-status:
	kubectl get pods,svc,ingress,hpa,pdb -n banking-system -o wide

k8s-down:
	kubectl delete -f k8s/ --ignore-not-found=true

helm-lint:
	helm lint deploy/helm/global-wallet
	helm lint deploy/helm/global-wallet -f deploy/helm/global-wallet/values-staging.yaml
	helm lint deploy/helm/global-wallet -f deploy/helm/global-wallet/values-prod.yaml

helm-template:
	helm template global-wallet deploy/helm/global-wallet

helm-install:
	helm upgrade --install global-wallet deploy/helm/global-wallet --namespace banking-system --create-namespace

helm-uninstall:
	helm uninstall global-wallet --namespace banking-system

