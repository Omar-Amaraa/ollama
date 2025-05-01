package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Omar-Amaraa/ollama/tree/main/convert"

	"github.com/ollama/ollama/api"
)

func main() {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}

	req := &api.GenerateRequest{
		Model:  "gemma2",
		Prompt: "how many planets are there?",

		// set streaming to false
		Stream: new(bool),
	}

	ctx := context.Background()

	// ------------------------------------------------------------
	// Callback qui filtre la réponse avant de l’afficher
	// ------------------------------------------------------------
	respFunc := func(resp api.GenerateResponse) error {
		// Vérifie d’abord si le texte contient un mot banni
		if convert.IsUnsafe(resp.Response) { // ← ajout
			return fmt.Errorf("unsafe content detected") // ← ajout
		}

		// Si tout est OK, on affiche
		fmt.Println(resp.Response)
		return nil
	}

	// L’appel Generate reste inchangé : le filtrage se fait dans respFunc
	if err := client.Generate(ctx, req, respFunc); err != nil {
		log.Fatal(err)
	}
}
