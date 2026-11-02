package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"immersive/internal/app"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	a, err := app.New()
	if err != nil {
		log.Fatal(err)
	}
	err = wails.Run(&options.App{
		Title:            "Immersive",
		Width:            1280,
		Height:           820,
		MinWidth:         900,
		MinHeight:        600,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 16, G: 16, B: 20, A: 1},
		OnStartup:        a.Startup,
		OnShutdown:       a.Shutdown,
		Bind:             []interface{}{a},
	})
	if err != nil {
		log.Fatal(err)
	}
}
