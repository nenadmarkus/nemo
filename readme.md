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

The image ships Go, Node.js (LTS), the .NET SDK, Python and a headless
Chromium. The toolchain state directories already point into the home volume
(`GOPATH`, `GOCACHE`, `NUGET_PACKAGES`, `NPM_CONFIG_PREFIX`, `PIPX_HOME`), so
installs work without touching the read-only rootfs; the Chromium bundles are
the exception and live on the image itself. To verify the wiring:

```
docker run --rm --read-only -v nemo-home:/home/nemo --entrypoint sh nemo -c \
  'go env GOPATH; npm config get prefix; dotnet --version; node --version'
```

## Headless browser

Chromium is installed by Playwright into `/opt/ms-playwright`, together with
the `playwright` CLI and Node library. Like the Go toolchain it lives on the
image rather than in the home volume, so it is shared by every container
started from the image and is not lost when the volume is recreated.

From a shell:

```
chromium --dump-dom https://example.com
chromium --screenshot=/work/page.png --window-size=1280,800 https://example.com
chromium --print-to-pdf=/work/page.pdf https://example.com
```

or, with the Playwright CLI:

```
playwright screenshot https://example.com /work/page.png
playwright pdf https://example.com /work/page.pdf
```

`chromium` is a small wrapper around the Playwright bundle that adds
`--headless --no-sandbox --disable-dev-shm-usage --disable-gpu --log-level=3`.
The Chromium sandbox cannot be used in this container (unprivileged user,
`--cap-drop=ALL`, `--security-opt=no-new-privileges`), and `/dev/shm` is only
64 MB, so a bare launch aborts or crashes. The container is the sandbox here;
run it with `--cap-drop=ALL` as above rather than giving the browser privileges
back. There is no D-Bus in the container either, so `--log-level=3` hides the
harmless errors Chromium logs on every run; pass `--log-level=0` to see them
again.

From Node, Playwright launches the browser with those flags on its own:

```js
// node shot.js
const { chromium } = require('playwright'); // NODE_PATH points at /usr/local/lib/node_modules

(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  await page.goto('https://example.com');
  await page.screenshot({ path: '/work/example.png', fullPage: true });
  console.log(await page.title());
  await browser.close();
})();
```

`NODE_PATH` is only consulted by `require`, so an ES module script has to
import the package from a local `node_modules` (see below) instead.

The Playwright version is pinned in the Dockerfile, because a browser bundle
only runs with the Playwright release that installed it. To get another browser
or another version, install it into the home volume:

```
PLAYWRIGHT_BROWSERS_PATH=$HOME/.cache/ms-playwright playwright install firefox
```

`PLAYWRIGHT_BROWSERS_PATH` is already exported in the image, so the assignment
is what keeps the download out of the read-only `/opt`; export it for later
runs of that browser as well. A project pinned to a different Playwright
version installs its own `playwright` package and its own bundles the same way.

To verify the browser works under the hardened flags:

```
docker run --rm --read-only --tmpfs /tmp:rw,exec,nosuid --cap-drop=ALL \
  --security-opt=no-new-privileges:true -v nemo-home:/home/nemo \
  --entrypoint chromium nemo --dump-dom 'data:text/html,<p>ok</p>'
```
