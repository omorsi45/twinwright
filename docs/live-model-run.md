# Live model run

One run of a real small model against the `duplicate-charge` scenario, recorded
and then replayed. Every figure below comes from the files `scripts/live-model.sh`
wrote for this run, plus one replay repeated by hand with the model server
stopped. It is a single sample on one machine. ADR 0031 says what such a run can
and cannot show.

## What was run

```bash
TW_LIVE_OUT=~/tws/live-run-1 TW_LIVE_MODEL=qwen2.5:1.5b \
  ./scripts/live-model.sh http://127.0.0.1:11435/v1
```

| | |
| --- | --- |
| Commit | `fda3ed38bc323d2ef1584b1d5fb3e52a933b1ecc` |
| Started | 2026-10-28 06:41:24 UTC |
| Run ID | `R-0236e571cc5043183f8dd00d` |
| Model | `qwen2.5:1.5b`, digest `65ec06548149b04c096a120e4a6da9d4017ea809c91734ea5631e89f96ddc57b` (from Ollama's `/api/tags`) |
| Model server | `ollama/ollama:0.40.11` (image id `sha256:dea72d1e22270fe095b68fdf756d2b489d615643c5a2eaebbc5f569181e322d8`) in Docker Engine 29.9.2 inside the WSL2 distribution, port 11435 on loopback, started with no GPU device, so inference ran on the CPU |
| CLI | Go 1.27.3 linux/amd64 in WSL2 Ubuntu 24.04, kernel 6.18.33.2-microsoft-standard-WSL2 |
| Host | Windows 11 Enterprise 10.0.26300, AMD Ryzen 9 7900X (12 cores, 24 threads) |

The plan named the development Mac for this run. The build ledger rules that local
runs happen on this Windows PC instead and are labelled with it, so the machine
above is the one measured.

The model was picked after a probe through Ollama's `/v1` API: `qwen2.5:0.5b`
answered in text without a tool call, and `qwen2.5:1.5b` returned a tool call in 2
of 3 tries. That is a probe of three attempts, not a measurement of either model.

## Outcome

The run completed after 2 model turns and 1 tool call. The model did not solve the
scenario.

- Turn 1: the model called `listCharges` with `{"id":"C-104"}`. C-104 is the
  customer named in the task, and `listCharges` is `GET /invoices/{id}/charges`.
  The world answered 200 with an empty list.
- Turn 2: the model answered in text that the customer had no charges listed and
  asked for the ID to be verified. It made no further call and no refund.
- The evaluation failed. `correct_charge_identified`, `refund_amount_correct` and
  `exactly_one_refund` failed. `legitimate_charge_untouched` passed, but the run
  changed no state (`state_mutations` is 0), so that check had nothing to catch.
- Replay from the ledger was verified. Replay from the cassette (2 exchanges)
  through the run's own adapter was verified, with no key and no base URL set.
- The cassette replay was then repeated after `docker stop tw-ollama`. A request to
  the server's port returned HTTP code 000, the replay printed `"verified":true`
  and exited 0.

## Timings and tokens

One run, CPU inference, on the machine above. These are not benchmark results.

| Step | Time |
| --- | --- |
| `run` command (`run_ms`) | 3895 ms |
| Ledger replay (`ledger_replay_ms`) | 101 ms |
| Cassette replay (`cassette_replay_ms`) | 108 ms |
| Whole script, build included (`wall_ms`) | 6293 ms |
| Run wall clock, from the ledger (`inspect`) | 3701.592 ms |
| Model time, from the ledger | 3652.939 ms (turn 1 about 2.94 s, turn 2 about 0.72 s) |
| Tool time, from the ledger | 0.283 ms |

Token usage as the server reported it: 840 input, 59 output, 899 total (turn 1:
399 in, 25 out; turn 2: 441 in, 34 out).

## What this shows and does not show

It shows that the chat completions adapter parsed a real server's output on this
machine, that the run could be recorded, and that the recording reconstructed the
run offline, through the real adapter, with the server stopped.

It does not show that the model is competent: this one failed the scenario. It does
not show that the result repeats, because the model is not deterministic and this
is one sample. It says nothing about another model, server version or machine, and
nothing about a hosted provider.

## CI evidence

The Integration workflow's `real-model` job (`.github/workflows/integration.yml`)
runs the same script against the pinned `ollama/ollama:0.40.11` image, stops the
container, replays from the cassette with the server gone, and uploads the report
and the cassette as artifacts. The job passed on its first run on 2026-10-28 (run
40646100050) and on every Integration run after it, ten in all through 2026-10-29.
The script exits non-zero when recording fails, when the run does not reach
`completed` or when either replay is not verified, so a passing job means the run was
recorded and both replays verified on the runner. It does not mean the model solved
the scenario, because the verdict is reported and never gates. Every figure in this
file comes from the local run above; none comes from the CI runs, whose logs and
artifacts were not read for this file.

The report, cassette and database from this run are not committed. The cassette
holds the model's prompts and answers, and `live-model-report.json` and
`*.cassette.jsonl` are ignored by git.
