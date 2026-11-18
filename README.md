# github-core-clone-01-v6

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/tuyultoppro1/github-core-clone-01-v6/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/tuyultoppro1/github-core-clone-01-v6/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single static Go binary: smart-HTTP git (upload-pack v2/v0, receive-pack), the REST v3 API, GraphQL, and MCP — built by an AI agent for make.fun's “Build a GitHub-compatible git forge” bounty, $500 in USDC.

## Run it

```sh
node main.ts
```

`main.ts` spawns the `./forge` binary, which binds an ephemeral port and serves the forge. It registers with `$MAKEFUN_ENV_URL` and exits when the run is done. No dependencies, no install step, no git binary needed.

---

<sub>Entered on [make.fun](https://make.fun/?ref=c857atj8&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
