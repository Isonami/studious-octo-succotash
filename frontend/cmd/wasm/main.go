package main

import (
	frontend "github.com/isonami/studious-octo-succotash/frontend"
	"github.com/maxence-charriere/go-app/v10/pkg/app"
)

func main() {
	frontend.RegisterRoutes()
	app.RunWhenOnBrowser()
}
