```
go run cmd/nemo/main.go --config "$(cat ~/.nemo/config.json)" --task "read table.png and transform it into markdown"
```

## Containerization

`docker build -t nemo .`

The container runs with a read-only root filesystem. The writable locations are:

- `/work` — the host workspace, bind-mounted read-write
- `/home/nemo` — a named volume for toolchain state and user-level installs:
  Go modules and `go install` binaries, NuGet packages, `dotnet tool`s,
  globally installed npm packages, pipx packages and venvs
- `/tmp` — a tmpfs for scratch files and built binaries

Because `/home/nemo` is a named volume, installs survive between runs and
`docker run --rm` does not throw them away. Remove the volume to start from a
clean toolchain:

```
docker volume rm nemo-home
```

```
docker run --rm -it \
  --network=bridge \
  --read-only \
  --tmpfs /tmp:rw,exec,nosuid \
  --cap-drop=ALL \
  --security-opt=no-new-privileges:true \
  -v nemo-home:/home/nemo:rw \
  -v "$PWD/work:/work:rw" \
  -v "$HOME/.nemo/config.json:/config.json:ro" \
  nemo
```

`/config.json` is a read-only bind mount of the host config, independent of the
home volume; it is mounted `:ro` and nothing in the container writes to it.

The image ships Go, Node.js (LTS), the .NET SDK and Python. Their state
directories already point into the home volume (`GOPATH`, `GOCACHE`,
`NUGET_PACKAGES`, `NPM_CONFIG_PREFIX`, `PIPX_HOME`), so installs work without
touching the read-only rootfs. To verify the wiring:

```
docker run --rm --read-only -v nemo-home:/home/nemo --entrypoint sh nemo -c \
  'go env GOPATH; npm config get prefix; dotnet --version; node --version'
```
