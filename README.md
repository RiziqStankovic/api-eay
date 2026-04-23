CustomAI Gateway (Go)
=====================

Gateway kecil berbasis Go yang expose endpoint OpenAI-compatible:

- `POST /v1/chat/completions`

Lalu request tersebut diteruskan ke upstream Responses API (contoh: `https://cloudfren.com/backend-api/codex/responses`).
Output SSE upstream (`response.output_text.delta`) diubah ke format stream OpenAI chunk (`chat.completion.chunk`) agar kompatibel dengan LiteLLM. 

Struktur Proyek
---------------

- `cmd/server/main.go` - Entrypoint HTTP server dan env config.
- `internal/http/chat_completions.go` - Handler OpenAI `/v1/chat/completions`.
- `internal/cursor/client.go` - Upstream HTTP client + parser SSE event.
- `internal/types/openai.go` - Tipe request/response OpenAI-compatible. 

Konfigurasi Environment 
-----------

Buat `.env` dari template:

```bash
cp .env.example .env
```

Env yang dipakai:

- `PORT` (opsional): default `8002`.
- `CUSTOMAI_API_URL` (opsional): default `https://cloudfren.com/backend-api/codex/responses`.
- `CUSTOMAI_AUTH_TOKEN` (wajib jika upstream butuh auth): boleh isi token mentah atau `Bearer <token>`.
- `CUSTOMAI_REFRESH_TOKEN` (opsional): jika diisi, gateway akan refresh access token otomatis saat mendekati expiry atau saat upstream mengembalikan `token_expired`.
- `CUSTOMAI_TOKEN_EXPIRES_AT` (opsional): expiry access token dalam Unix milliseconds. Jika tidak diisi dan token berupa JWT, gateway akan baca claim `exp` otomatis.
- `CUSTOMAI_TOKEN_URL` (opsional): endpoint refresh token. Jika kosong dan access token JWT punya claim `iss`, gateway pakai `<iss>/oauth/token`.
- `CUSTOMAI_OAUTH_CLIENT_ID` (opsional): client ID untuk refresh. Jika kosong dan access token JWT punya claim `client_id`, gateway pakai nilai itu.
- `CUSTOMAI_TOKEN_SCOPES` (opsional): scopes refresh, bisa dipisah spasi atau koma.
- `CUSTOMAI_REFRESH_BUFFER_SECONDS` (opsional): refresh sebelum expired, default `300`.
- `CUSTOMAI_TOKEN_STORE_PATH` (opsional): file JSON untuk menyimpan access token + refresh token terbaru setelah rotasi. Default `.customai-tokens.json`.
- `CUSTOMAI_COOKIE` (opsional): jika upstream butuh cookie Cloudflare/session.
- `CUSTOMAI_UPSTREAM_ORIGIN` / `CUSTOMAI_UPSTREAM_REFERER` / `CUSTOMAI_UPSTREAM_USER_AGENT` / `CUSTOMAI_UPSTREAM_ACCEPT_LANGUAGE` (opsional): header tambahan umum untuk upstream.
- `CUSTOMAI_EXTRA_HEADERS` (opsional): header tambahan bebas, format `Key: Value||Key2: Value2`.
- `CUSTOMAI_TIMEOUT` (opsional): timeout request upstream dalam detik, default `180`.
- `CUSTOMAI_ALLOWED_MODELS` (opsional): daftar model yang diizinkan (comma-separated).
- `CUSTOMAI_GATEWAY_API_KEY` (opsional): jika di-set, endpoint gateway wajib `Authorization: Bearer <token>`.

Menjalankan Server
------------------

Di folder `customai-gateway-go/`:

```bash
go run ./cmd/server
```

Contoh Request
--------------

```bash
curl -X POST http://localhost:8002/v1/chat/completions \
  -H "Authorization: Bearer gateway-test-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5-codex",
    "messages": [
      {"role": "system", "content": "You are a helpful coding assistant."},
      {"role": "user", "content": "/status"}
    ],
    "stream": true
  }'
```

Integrasi ke LiteLLM
--------------------

Tambahkan model baru di `config.yaml` LiteLLM:

```yaml
model_list:
  - model_name: customai-codex
    litellm_params:
      model: openai/gpt-5-codex
      api_base: http://host.docker.internal:8002/v1
      api_key: ${CUSTOMAI_GATEWAY_API_KEY}
```

Catatan:

- LiteLLM akan memanggil endpoint OpenAI-compatible gateway ini.
- `model` yang diterima LiteLLM akan diteruskan ke upstream sebagai field `model`.
- Untuk request non-stream, gateway tetap consume stream upstream lalu menggabungkan delta jadi satu jawaban final.
- Jika upstream mengembalikan `token_expired`, gateway akan mencoba refresh token sekali lalu retry request.
- Jika provider merotasi `refresh_token`, gateway akan menyimpan token terbaru ke file store agar restart berikutnya tidak memakai token lama lagi.
