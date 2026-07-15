# HITL setup: Plane/GitHub decisions, one-way Feishu notifications

Looper keeps collaboration in the system of record:

- product specs, approvals, and product answers live in Plane;
- code, review, and implementation answers live in GitHub;
- Feishu only delivers targeted notifications and never accepts an answer;
- the authenticated `/respond` API is an operator/dashboard fallback.

There is no Feishu event subscription, callback route, Cloudflare inbox worker,
thread-reply mirror, or card-action receiver to deploy.

## Minimal configuration

```json
{
  "hitl": {
    "enabled": true,
    "answerTransport": "github",
    "github": {
      "awaitingLabel": "looper:awaiting-human",
      "mentionLogins": ["github-owner"],
      "answerAuthors": ["github-owner"]
    }
  },
  "notifications": {
    "webhook": {
      "enabled": true,
      "mode": "app",
      "format": "feishu",
      "appIdEnv": "LOOPER_FEISHU_APP_ID",
      "appSecretEnv": "LOOPER_FEISHU_APP_SECRET",
      "chatId": "oc_xxx",
      "mentionOpenIds": ["ou_xxx"]
    }
  }
}
```

Only the two app credentials are secrets:

```sh
export LOOPER_FEISHU_APP_ID=cli_xxx
export LOOPER_FEISHU_APP_SECRET=xxx
```

`chatId`, GitHub logins, and Feishu open IDs are identifiers and may remain in the
config. Keep the credentials in the service environment, never in the JSON file.

## Runtime flow

1. An agent surfaces a genuine blocker.
2. Looper creates the exact question/comment in Plane or GitHub and persists a
   named blocked condition.
3. A one-owner Feishu card points to that exact source location.
4. The human follows the link and answers there.
5. The source-of-truth watcher observes the answer, clears the condition, and
   resumes the same loop/session.

Posting in the Feishu thread, clicking old interactive buttons, or mentioning a
bot does nothing by design.

## Operator fallback

For local testing or a dashboard integration, an authenticated operator may use:

```sh
curl -X POST http://127.0.0.1:7788/api/v1/loops/<seq>/respond \
  -H 'Content-Type: application/json' \
  -d '{"answer":"approved option"}'
```

This route is governed by `server.authMode`. It is not a Feishu integration.

## Smoke test

Use an isolated Looper home and test repository. Trigger a task that asks a
question, then verify:

- the question exists in Plane/GitHub;
- the Feishu card contains the expected owner and exact deep link;
- `POST /api/v1/hitl/feishu` returns 404;
- a Feishu thread reply does not change loop state;
- answering in Plane/GitHub resumes the loop.
