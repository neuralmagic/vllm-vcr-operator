#!/usr/bin/env python3
"""Generate the mock profile's latency trace.

This is deliberately synthetic: 40 records with plausible TTFT/ITL for a small
model on an H200, seeded so the file is reproducible. It gives the simulator
realistic pacing for protocol and routing tests. It is NOT a recording and
supports no timing-fidelity claim; see README.md.
"""
import gzip
import json
import random
import sys

BLOCK_SIZE = 64
SEED = 7


def main(path: str) -> None:
    rng = random.Random(SEED)
    records = []
    for _ in range(40):
        prompt = rng.choice([64, 128, 256, 512, 1024, 2048])
        output = rng.choice([8, 16, 32, 64])
        ttft = 20.0 + prompt * 0.02 + rng.uniform(0, 5)
        itl = [round(8.0 + rng.uniform(0, 4), 2) for _ in range(output - 1)]
        records.append({
            "prompt_tokens": prompt,
            "output_tokens": output,
            "ttft_ms": round(ttft, 2),
            "itl_ms": itl,
            "concurrency": rng.choice([1, 1, 2, 4]),
        })
    with gzip.open(path, "wt") as handle:
        for record in records:
            handle.write(json.dumps(record) + "\n")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "profile/mock-trace.jsonl.gz")
