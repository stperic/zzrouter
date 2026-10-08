//go:build ignore

// Generates icon PNG files for Windows resource embedding.
// Run: go run winres/generate_icon.go
package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

func main() {
	sizes := map[string]int{
		"winres/icon.png":   48,
		"winres/icon16.png": 16,
	}
	// Brand color: deep blue (#1a56db)
	bg := color.RGBA{R: 26, G: 86, B: 219, A: 255}
	fg := color.RGBA{R: 255, G: 255, B: 255, A: 255}

	for path, size := range sizes {
		img := image.NewRGBA(image.Rect(0, 0, size, size))
		// Fill background with rounded-corner-ish solid color
		for y := range size {
			for x := range size {
				img.Set(x, y, bg)
			}
		}
		// Draw a simple "Z" shape scaled to the icon size
		drawZ(img, size, fg)
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, img); err != nil {
			panic(err)
		}
		f.Close()
	}
}

func drawZ(img *image.RGBA, size int, fg color.RGBA) {
	margin := size / 6
	if margin < 2 {
		margin = 2
	}
	thick := size / 8
	if thick < 1 {
		thick = 1
	}
	left := margin
	right := size - margin
	top := margin
	bottom := size - margin

	// Top bar
	for y := top; y < top+thick; y++ {
		for x := left; x < right; x++ {
			img.Set(x, y, fg)
		}
	}
	// Bottom bar
	for y := bottom - thick; y < bottom; y++ {
		for x := left; x < right; x++ {
			img.Set(x, y, fg)
		}
	}
	// Diagonal
	height := bottom - top
	width := right - left
	for i := range height {
		x := right - margin/2 - (i * width / height)
		for dx := range thick {
			if x+dx >= left && x+dx < right {
				img.Set(x+dx, top+i, fg)
			}
		}
	}
}
