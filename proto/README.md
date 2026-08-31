# PROTOTYPE — what is left of it

One open question, and these two probes are its instrument: **can a run start
Docker inside its own sandbox?** That is [issue #28][28], and ADR 0001 leaves it
explicitly unmeasured — *"whether bubblewrap works inside `rootlesskit`"* must be
settled before the wrap is used for real.

- `dind.sh` — Docker in a gVisor sandbox, as uid 1000, unprivileged.
- `dind-net.sh` — whether an inner container is reachable from the agent process.

Both refuse any API server that is not the local k3s, and both need a
`gvisor`/`gvisor-dind` RuntimeClass. The host setup is in
[docs/research/prototype-findings.md][rec].

Everything else the prototype answered has shipped — the base image and the run
plan as `sandbox/`, the credential proxy as `proxy/`, the Job as
`charts/orchestrator` and `orchestrator/`. **The measurements themselves are kept**,
in [docs/research/prototype-findings.md][rec], because the ADRs cite them as their
evidence.

[28]: https://github.com/nissessenap/the-implementer/issues/28
[rec]: ../docs/research/prototype-findings.md
