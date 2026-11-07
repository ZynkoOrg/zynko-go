// Ping: answers "!ping" with "pong".
//
//	ZYNKO_TOKEN=bot_... go run ./examples/ping
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"zynko.org/go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := zynko.New(zynko.Options{
		Token:   os.Getenv("ZYNKO_TOKEN"),
		API:     os.Getenv("ZYNKO_API"), // another server; https://zynko.org/api by default
		Intents: zynko.IntentGuilds | zynko.IntentMessages,
	})
	if err != nil {
		log.Fatal(err)
	}

	client.OnMessageCreate(func(ctx context.Context, m *zynko.Message) error {
		if m.Mine() || m.Content != "!ping" {
			return nil
		}
		_, err := m.Reply(ctx, "pong")
		return err
	})
	client.OnError(func(err error) { log.Print(err) })

	me, err := client.Login(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%s conectado em %d servidores", me.DisplayName, len(client.Guilds()))

	<-ctx.Done()
	client.Close()
}
