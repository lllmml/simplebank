postgres:
	docker run --name postgres18 -p 5432:5432 -e POSTGRES_USER=root -e POSTGRES_PASSWORD=314159 -d postgres:18-alpine

stop:
	docker stop postgres18

start:
	docker start postgres18

createdb:
	docker exec -it postgres18 createdb --username=root --owner=root simple_bank

dropdb:
	docker exec -it postgres18 dropdb simple_bank

sqlc:
	sqlc generate 

test:
	go test -v -cover ./...

server:
	go run main.go
migrateup:
	migrate -path db/migration -database "postgresql://root:314159@localhost:5432/simple_bank?sslmode=disable" -verbose up

migratedown:
	migrate -path db/migration -database "postgresql://root:314159@localhost:5432/simple_bank?sslmode=disable" -verbose down

.PHONY: createdb dropdb postgres migrateup migratedown test sqlc server stop start