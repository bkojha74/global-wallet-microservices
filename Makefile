.PHONY: up up-mongodb up-app down down-mongodb down-app logs test clean k8s-build k8s-deploy

up: up-mongodb
	docker compose -f docker-compose.yml up --build -d
	@echo "Services are spinning up. Run 'make logs' or wait 10s then 'make test'."

up-mongodb:
	docker network inspect wallet_shared_net >/dev/null 2>&1 || docker network create wallet_shared_net
	docker volume inspect global-wallet-microservices_mongo_data >/dev/null 2>&1 || docker volume create global-wallet-microservices_mongo_data
	docker compose -f docker-compose.mongodb.yml up -d

up-app:
	docker compose -f docker-compose.yml up --build -d

down:
	docker compose -f docker-compose.yml down
	docker compose -f docker-compose.mongodb.yml down

down-app:
	docker compose -f docker-compose.yml down

down-mongodb:
	docker compose -f docker-compose.mongodb.yml down

logs:
	docker compose -f docker-compose.yml logs -f

test:
	@chmod +x scripts/test-e2e.sh
	./scripts/test-e2e.sh

clean:
	docker compose -f docker-compose.yml down -v --rmi all
	docker compose -f docker-compose.mongodb.yml down -v
	docker volume rm global-wallet-microservices_mongo_data

k8s-deploy:
	kubectl apply -f k8s/
