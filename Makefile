.PHONY: up up-mongodb up-queue up-auth up-logging up-monitoring up-quality up-app down down-mongodb down-queue down-auth down-logging down-monitoring down-quality down-app logs test clean k8s-build k8s-deploy sonar-scan sast-scan

up: up-mongodb up-queue up-auth up-logging
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
	docker compose -f docker-compose.logging.yml down
	docker compose -f docker-compose.auth.yml down
	docker compose -f docker-compose.monitoring.yml down
	docker compose -f docker-compose.rabbitmq.yml down
	docker compose -f docker-compose.mongodb.yml down

down-app:
	docker compose -f docker-compose.yml down

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
	docker compose -f docker-compose.monitoring.yml down -v
	docker compose -f docker-compose.logging.yml down -v
	docker compose -f docker-compose.auth.yml down -v
	docker compose -f docker-compose.rabbitmq.yml down -v
	docker compose -f docker-compose.mongodb.yml down -v
	docker volume rm global-wallet-microservices_mongo_data

k8s-deploy:
	kubectl apply -f k8s/
