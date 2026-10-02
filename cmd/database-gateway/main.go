package main

import (
	"github.com/dialohq/tailscale-database-gateway/internal/database"
	"github.com/dialohq/tailscale-database-gateway/internal/service"
)

func main() { service.Main(database.Run) }
