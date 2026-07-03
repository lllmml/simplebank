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

mock:
	mockgen -package mockdb -destination db/mock/store.go github.com/lllmml/simplebank/db/sqlc Store

migrateup:
	migrate -path db/migration -database "postgresql://root:314159@localhost:5432/simple_bank?sslmode=disable" -verbose up

migrateup1:
	migrate -path db/migration -database "postgresql://root:314159@localhost:5432/simple_bank?sslmode=disable" -verbose up 1


migratedown:
	migrate -path db/migration -database "postgresql://root:314159@localhost:5432/simple_bank?sslmode=disable" -verbose down

migratedown1:
	migrate -path db/migration -database "postgresql://root:314159@localhost:5432/simple_bank?sslmode=disable" -verbose down 1


.PHONY: createdb dropdb postgres migrateup migratedown migrateup1 migratedown1 test sqlc server stop start mock