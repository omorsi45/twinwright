#!/usr/bin/env python3
"""Reference agent for Twinwright's exec protocol, twinwright.agent.v1.

This is a protocol example, not a model. It reads one JSON request from stdin,
decides what to do next from the tool results already in the history, and writes
one JSON response to stdout. It solves the duplicate-charge scenario the same
way every time, using only the Python standard library.

The decision depends on nothing but the history it is sent, so Twinwright can
start it afresh for every turn and resume a run in another process.

Usage: billing_agent.py [--echo-request]

  --echo-request   also write the request it received to stderr, for debugging
"""

import json
import sys

PROTOCOL = "twinwright.agent.v1"


def tool_call(call_id, operation_id, arguments):
    """A response that proposes one call and says nothing else."""
    return {
        "protocol": PROTOCOL,
        "content": "",
        "tool_calls": [{"id": call_id, "operation_id": operation_id, "arguments": arguments}],
    }


def final_answer(text):
    """A response with no calls ends the run with this text."""
    return {"protocol": PROTOCOL, "content": text, "tool_calls": []}


def decide(history):
    """Read the customer, their invoice and its charges, refund the duplicate."""
    tools = [message for message in history if message.get("role") == "tool"]
    done = len(tools)
    # Call IDs must be unique within a run, so each is derived from the step.
    call_id = "py-%d" % (done + 1)

    if done == 0:
        return tool_call(call_id, "getCustomer", {"id": "C-104"})
    if done == 1:
        return tool_call(call_id, "listInvoices", {"id": "C-104"})
    if done == 2:
        return tool_call(call_id, "listCharges", {"id": "INV-104"})

    last = tools[-1]
    operation, status = last.get("operation_id"), last.get("status")
    if operation == "listCharges" and status == 503:
        # A 503 is temporary: ask again under a new call ID.
        return tool_call(call_id, "listCharges", {"id": "INV-104"})
    if operation == "listCharges" and status == 200:
        charges = json.loads(last["content"])
        if len(charges) != 2:
            raise ValueError("expected two charges on the invoice, got %d" % len(charges))
        duplicate = charges[1]
        return tool_call(call_id, "createRefund", {
            "charge_id": duplicate["id"],
            "amount_cents": duplicate["amount_cents"],
            "reason": "duplicate charge",
        })
    if operation == "createRefund" and status == 201:
        return final_answer("Duplicate charge refunded.")
    raise ValueError("cannot continue after %s HTTP %s" % (operation, status))


def main(argv):
    flags = argv[1:]
    if any(flag != "--echo-request" for flag in flags):
        sys.stderr.write(__doc__)
        return 2

    raw = sys.stdin.read()
    if "--echo-request" in flags:
        sys.stderr.write(raw)
        if not raw.endswith("\n"):
            sys.stderr.write("\n")

    try:
        request = json.loads(raw)
        if request.get("protocol") != PROTOCOL:
            raise ValueError("unsupported protocol %r" % (request.get("protocol"),))
        response = decide(request.get("history", []))
    except (ValueError, KeyError, TypeError) as error:
        sys.stderr.write("billing_agent: %s\n" % error)
        return 2

    sys.stdout.write(json.dumps(response) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
