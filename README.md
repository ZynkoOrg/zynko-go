# zynko (Go)

Biblioteca oficial para bots do [Zynko](https://zynko.org) em Go: gateway com reconexão,
caches de servidores, canais e membros, eventos tipados, a API inteira, comandos com "/",
cartões e botões, e voz (tocar e ouvir) sem cgo.

```sh
go get zynko.org/go
```

```go
client, _ := zynko.New(zynko.Options{Token: os.Getenv("ZYNKO_TOKEN")})
client.OnMessageCreate(func(ctx context.Context, m *zynko.Message) error {
	if m.Content == "!ping" {
		_, err := m.Reply(ctx, "pong")
		return err
	}
	return nil
})
client.Login(ctx)
```

A voz fica no pacote `zynko.org/go/voice` (precisa do [ffmpeg](https://ffmpeg.org) instalado).
Exemplos em [`examples/`](examples): `ping`, `dado` (comandos, cartão e botão) e `musica`.

Documentação completa: **[docs.zynko.org/bots/go](https://docs.zynko.org/bots/go)**.
