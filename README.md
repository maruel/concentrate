# concise

Pipe any command into `concise` with a question; it summarises the output using
an LLM and writes a short answer to stdout.

```sh
gh run view --log |& concise "why did the workflow fail?"
git diff          |  concise "what changed?"
rg TODO           |  concise "extract TODOs as JSON"
go test ./...     |& concise "did the tests pass?"
```

## Install

```sh
go install github.com/maruel/concentrate/cmd/concise@latest
```

## Configuration

| Variable           | Description                                          |
|--------------------|------------------------------------------------------|
| `CONCISE_PROVIDER` | Provider name: `anthropic`, `gemini`, `ollama`, …    |
| `CONCISE_MODEL`    | Model name or alias: `CHEAP`, `GOOD`, `SOTA`         |
| `CONCISE_REMOTE`   | Custom provider endpoint URL                         |
| `CONCISE_TIMEOUT`  | Request timeout (e.g. `30s`, `2m`); default `90s`    |

`CONCISE_PROVIDER` is required unless only one provider is configured. Run
`concise -help` to see all flags; flags override environment variables.

## How it works

`concise` classifies the incoming byte stream and picks a strategy:

- **Batch** — waits for EOF, then asks the LLM to summarise the full output.
- **Watch** — detected when successive bursts are structurally similar (e.g.
  `watch`, `kubectl get pods -w`); each cycle pair is diffed and summarised.
- **Interactive** — detected when the stream tail looks like a prompt
  (`[y/N]`, `password:`, …); input is passed through verbatim.

If the LLM produces a bad summary the original output is written to stdout
unchanged, so the tool is always safe to use in pipelines.
