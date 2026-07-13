package gapi

import (
	"fmt"

	db "github.com/lllmml/simplebank/db/sqlc"
	"github.com/lllmml/simplebank/pb"
	"github.com/lllmml/simplebank/token"
	"github.com/lllmml/simplebank/util"
	"github.com/lllmml/simplebank/worker"
)

// Server serves HTTP requests for our banking service.
type Server struct {
	pb.UnimplementedSimpleBankServer
	config util.Config
	store db.Store
	tokenMaker token.Maker
	taskDistributor worker.TaskDistributor
}

// NewServer creates a new HTTP server and setup routing.
func NewServer(config util.Config, store db.Store, taskDistributor worker.TaskDistributor) (*Server, error) {
	tokenMaker, err := token.NewPasetoMaker(config.TokenSymmetricKey)
	if err != nil {
		return nil, fmt.Errorf("cannot create token maker: %w", err)
	}
	server := &Server{
		config: config,
		store: store,
		tokenMaker: tokenMaker,
		taskDistributor: taskDistributor, 
	}

	return server, nil
}