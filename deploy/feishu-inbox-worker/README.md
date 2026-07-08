# Feishu → looper event inbox (Cloudflare Worker)

A **shared** Feishu app can't use long-connection (events go to one connection, so
N loopers would steal each other's). This dumb, durable inbox lets one shared app
deliver inbound events (HITL thread replies + card-action clicks) to many local
loopers behind NAT: Feishu POSTs here, each looper **polls** here and self-selects
the events whose thread it owns (via its local `feishu_threads` map).

## Deploy
```bash
cd deploy/feishu-inbox-worker
wrangler d1 create looper-feishu-inbox          # paste the id into wrangler.toml
wrangler d1 execute looper-feishu-inbox --file=schema.sql --remote
wrangler secret put FEISHU_VERIFY_TOKEN         # app's Event Subscription Verification Token
wrangler secret put POLL_TOKEN                  # a random shared secret for loopers
wrangler deploy
```

## Wire the shared Feishu app
- Event Subscription → **Request URL** = the deployed Worker URL (`https://…workers.dev/`).
- Subscribe to `im.message.receive_v1`; point the card **callback URL** at the same URL.
- **Turn Encrypt Key OFF** (the inbox reads plain JSON).
- Copy the **Verification Token** into `FEISHU_VERIFY_TOKEN`.

## Wire each looper
```jsonc
"hitl": {
  "enabled": true,
  "answerTransport": "feishu",
  "feishu": {
    "inbound": "cf-inbox",
    "eventInboxUrlEnv": "LOOPER_FEISHU_INBOX_URL",     // https://…workers.dev/events
    "eventInboxTokenEnv": "LOOPER_FEISHU_INBOX_TOKEN"  // = POLL_TOKEN
  }
}
```

## Contract
- `POST /` — Feishu callback. Verifies `FEISHU_VERIFY_TOKEN`, answers
  `url_verification`, stores `im.message.receive_v1` (text) + card actions.
- `GET /events?since=<id>&limit=<n>` — `Authorization: Bearer <POLL_TOKEN>`;
  returns `{ok, events:[{id,kind,rootId,senderOpenId,text,value,…}], cursor}`.
  Poll with the last `cursor` as `since`.
