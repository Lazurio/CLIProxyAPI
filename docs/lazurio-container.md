# Lazurio server container

The fork's **Lazurio container** workflow builds a selected, already merged
commit for Linux amd64 and publishes it to `ghcr.io/lazurio/cliproxyapi`.
It uses the repository-scoped Actions package permission, not the upstream
Docker Hub credentials. It does not refresh the model catalog during the build
or include any local configuration, account files or conversation data.

Dispatch from `main` with an exact `source_commit` and a Lazurio `version`:

```sh
gh workflow run lazurio-container.yml --repo Lazurio/CLIProxyAPI --ref main \
  -f source_commit=395e7206770035162feabc581bcaa227a1eb0f58 \
  -f version=8.0.30-lazurio.thread3
```

The workflow refuses commits outside `main`'s history. The job summary records
the source, version and registry digest. A tag is a discovery aid; deployments
must select the resulting `@sha256:` reference in the Machines software catalog,
after qualification with the actual service configuration. Never promote an
unqualified digest or substitute a mutable tag into a deployment.

GitHub creates new packages as private. Before the first deployment, an owner
must make this public-source container package public and verify anonymous
pulls. No registry login is installed on the services guest. The workflow does
not change package access settings or upstream release workflows.

Qualification must use synthetic credentials. Live OAuth accounts remain in
their single owning proxy service: copying them into a second running instance
can invalidate the original refresh token. Back up the existing account volume,
stop the old container before starting the replacement, and verify account
counts, health and the administration UI. A rollback uses the previous qualified
image with the same persistent account volume.
