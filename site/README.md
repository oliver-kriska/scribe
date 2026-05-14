# site/

Marketing site for scribe, served at <https://getscribe.dev> via Cloudflare
Workers (static assets, no Worker code).

## Layout

```
site/
├── wrangler.toml      # Workers config (assets-only, custom-domain routes)
├── public/
│   └── index.html     # the page itself — single self-contained HTML, no JS deps in repo
└── README.md
```

The repo intentionally contains **only HTML/CSS** — no `package.json`,
`node_modules`, or build step. Wrangler is installed globally on the dev
machine, not as a project dependency, so the supply-chain surface of this
repo stays at zero npm packages.

Anything under `public/` is uploaded as a static asset. Add images, more
pages, or a `favicon.ico` there.

## One-time setup

Install wrangler globally (skip postinstall scripts to avoid the `sharp`
native build):

```sh
npm install --global --ignore-scripts wrangler@latest
```

Credentials live in repo-root `.env` (gitignored):

```
export CLOUDFLARE_API_TOKEN="..."
export CLOUDFLARE_ACCOUNT_ID="<REDACTED>"
```

## Deploy

```sh
set -a; source ../.env; set +a       # load creds into the shell
cd site
wrangler deploy
```

First deploy attaches `getscribe.dev` and `www.getscribe.dev` as custom
domains; subsequent deploys just push new asset hashes (no DNS edits needed).

## Iterate locally

```sh
cd site
wrangler dev                          # serves on http://localhost:8787
```

## Tail edge logs

```sh
cd site
wrangler tail
```
