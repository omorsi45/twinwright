# Python reference agent

`billing_agent.py` is a protocol example, not a model. It speaks Twinwright's
exec protocol (`twinwright.agent.v1`, see `docs/protocol/agent-v1.md`) and solves
the `duplicate-charge` scenario the same way every time, from the tool results in
the history it is sent. It uses only the Python standard library.

Use it as a starting point for an agent of your own: read one JSON request from
stdin, write one JSON response to stdout, exit 0.

## Run it

Build the billing world once, then run the scenario with the agent as the
decision maker. `--model` is required and names your agent and its version; it is
recorded with the run.

```sh
twinwright build examples/billing/openapi.yaml --bindings examples/billing/bindings.yaml --out twinwright.manifest.json

twinwright run duplicate-charge \
  --agent exec \
  --model billing-agent@1 \
  --agent-command 'python3 examples/agents/python/billing_agent.py' \
  --fail-on-eval
```

`--agent-command` is split into arguments without a shell: words are separated by
blanks, and single or double quotes group a word that contains blanks. The agent
gets a minimal environment (`PATH` and `HOME`); pass a variable with
`--agent-env NAME`, which hands the agent the CLI's own value of `NAME`.

The command is not stored with the run. Resuming a paused run, or serving it from
a worker, needs `--agent-command` again.

## Try it by hand

```sh
echo '{"protocol":"twinwright.agent.v1","task":"t","history":[],"operations":[]}' \
  | python3 examples/agents/python/billing_agent.py --echo-request
```

`--echo-request` also writes the request it received to stderr, which is the
quickest way to see what an agent is sent.
