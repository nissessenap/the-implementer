# The implementer

It's an GitHub webhook which triggers AI agents to implement a specific feature based on a label or a tag of the GitHub app user.

It's built on top of the philisophy of Matt Pocock [skills](https://github.com/mattpocock/skills), where the developre is supposed to run `/grill-with-docs` until all questions has been answered and from there relevant issues has been created.

But instead of running `/implement` on your laptop the implementer will do it for you.

The plan is to use [scion](https://github.com/GoogleCloudPlatform/scion) to distribute the workload to Kubernetes and using the CNCF project [agent-sandbox](https://agent-sandbox.sigs.k8s.io/) for isolation.

Your organization probably has multiple different langauges and it's up to you to setup what should be pre-installed in each sandbox container, another option is also to run the container as root and install packages as you need it, but this will of course add time to each agent run.

## Roadmap

- Trigger implementation based on label
- Trigger implementation based on tag + extra context written in a comment in issue.
- Trigger feedback on a review using a comment in PR.

### Docs

- How to build a scion image ready for usage
- How to install scion and agent sandbox in k8s.
- How to install gvisor for improved security
- How to setup this webhook with a GitHub app

### Long term

- Get a direct link to scion so you can see what the agent is doing as a comment in the issue.
- State managment, should we be able to launch the existing agent again/interact with an agent to fix a code review or similar.
  If I remember correctly this possibility exist in scion.
