// Dado: a "/dado" command that answers with a card and a "De novo" button.
//
//	ZYNKO_TOKEN=bot_... go run ./examples/dado
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"zynko.org/go"
)

const lavanda = 0xa594f9

var commands = []zynko.Command{
	{
		Name:        "dado",
		Description: "Rola um dado",
		Options: []zynko.CommandOption{
			{Name: "lados", Description: "Quantos lados", Type: zynko.OptionInteger, MinValue: zynko.Limit(2), MaxValue: zynko.Limit(100)},
		},
	},
	{Name: "ping", Description: "Responde pong"},
}

// roll is the card and the button for one roll of a die with n sides.
func roll(n int64, by string) ([]zynko.Card, zynko.Components) {
	cards := []zynko.Card{{
		Title:       fmt.Sprintf("🎲 %d", 1+rand.Int64N(n)),
		Description: fmt.Sprintf("Dado de %d lados, rolado por %s", n, by),
		Color:       lavanda,
	}}
	buttons := zynko.Components{{
		{Style: zynko.ButtonPrimary, Emoji: "🎲", Label: "De novo", CustomID: "dado:" + strconv.FormatInt(n, 10)},
	}}
	return cards, buttons
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := zynko.New(zynko.Options{Token: os.Getenv("ZYNKO_TOKEN"), API: os.Getenv("ZYNKO_API")})
	if err != nil {
		log.Fatal(err)
	}

	client.OnInteraction(func(ctx context.Context, i *zynko.Interaction) error {
		switch {
		case i.Command == "ping":
			_, err := i.ReplyEphemeral(ctx, "pong")
			return err
		case i.Command == "dado":
			lados, ok := i.Int("lados")
			if !ok {
				lados = 6
			}
			cards, buttons := roll(lados, i.User.DisplayName)
			_, err := i.ReplyWith(ctx, zynko.ReplyOptions{Cards: cards, Components: buttons})
			return err
		case i.IsButton() && strings.HasPrefix(i.CustomID, "dado:"):
			lados, _ := strconv.ParseInt(strings.TrimPrefix(i.CustomID, "dado:"), 10, 64)
			cards, buttons := roll(max(2, lados), i.User.DisplayName)
			_, err := i.UpdateWith(ctx, zynko.EditOptions{Cards: cards, Components: buttons})
			return err
		}
		return nil
	})
	client.OnError(func(err error) { log.Print(err) })

	if _, err := client.Login(ctx); err != nil {
		log.Fatal(err)
	}
	if _, err := client.SetCommands(ctx, commands); err != nil {
		log.Fatal(err)
	}
	log.Printf("pronto, com %d comandos", len(commands))

	<-ctx.Done()
	client.Close()
}
