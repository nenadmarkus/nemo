```
go run cmd/nemo/main.go --config "$(cat ~/.nemo/config.json)" --task "read table.png and transform it into markdown"
```

```
docker run --rm -it \
  --network=bridge \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid \
  --cap-drop=ALL \
  --security-opt=no-new-privileges:true \
  -v "$PWD/work:/work:rw" \
  -v "$HOME/.nemo/config.json:/config.json:ro" \
  nemo
```