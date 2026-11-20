# Mock profile image (integration only)

A contract-valid profile image whose trace is **synthetic**. It exists so the
operator, the real CPU vLLM frontend and the real vllm-vcr simulator can be
exercised end to end before any profile is recorded on GPUs.

What it does prove: profile parsing and validation, digest pinning, the
provisioning entrypoint, coverage enforcement, vocabulary-size translation, KV
event publication, readiness and routing.

What it does **not** prove: timing, throughput, or any model behaviour. The 40
records are seeded noise shaped like an H200 running a small model.

Build and push it like any other test image:

```bash
python3 test/images/mock-profile/generate-trace.py test/images/mock-profile/profile/mock-trace.jsonl.gz
VCR_IMAGE_PREFIX=<registry>/<namespace> hack/images.sh --stub
```
