package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/openclaw/customai-gateway-go/internal/authflow"
)

func main() {
	_ = godotenv.Load()

	var (
		storePath     = flag.String("store", envDefault("CUSTOMAI_TOKEN_STORE_PATH", "auth-profiles.json"), "profile store path")
		profile       = flag.String("profile", os.Getenv("CUSTOMAI_TOKEN_PROFILE"), "profile key or email to update")
		clientID      = flag.String("client-id", envDefault("CUSTOMAI_OAUTH_CLIENT_ID", authflow.DefaultCodexClientID), "OAuth client id")
		callback      = flag.Int("callback-port", authflow.DefaultCallbackPort, "preferred localhost callback port")
		openBrowser   = flag.Bool("open-browser", true, "open login URL in the default browser")
		pasteCallback = flag.Bool("paste-callback", true, "allow pasting the callback URL into the terminal")
	)
	flag.Parse()

	key, err := authflow.Login(context.Background(), authflow.Options{
		StorePath:     *storePath,
		Profile:       *profile,
		ClientID:      *clientID,
		CallbackPort:  *callback,
		OpenBrowser:   *openBrowser,
		PasteCallback: *pasteCallback,
	})
	if err != nil {
		log.Fatalf("auth failed: %v", err)
	}

	fmt.Printf("Saved Codex credentials to %s profile %s\n", *storePath, key)
	fmt.Printf("Use with:\nCUSTOMAI_TOKEN_STORE_PATH=%s\nCUSTOMAI_TOKEN_PROFILE=%s\n", *storePath, key)
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
