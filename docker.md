# Running Sundae with Docker

No Sundae image is published. Build one from the [`Containerfile`](Containerfile)
at the repository root (`docker build` and `podman build` take the same
arguments; [helm/sundae/README.md](helm/sundae/README.md) has the full build and push
commands):

```sh
docker build -f Containerfile -t sundae:dev .
```

Here’s how you might run `sundae` as a container. Keep in mind that
repositories are stored in the `/sundae` directory, so you’ll likely want
to mount that directory as a volume in order keep your repositories backed up.
The image runs as UID/GID 10001, so the volume must be writable by it.

```sh
docker run \
  --name=sundae \
  --volume /path/to/data:/sundae \
  --publish 23231:23231 \
  --publish 23232:23232 \
  --publish 23233:23233 \
  --publish 9418:9418 \
  -e SUNDAE_INITIAL_ADMIN_KEYS="YOUR_ADMIN_KEY_HERE" \
  --restart unless-stopped \
  sundae:dev
```

Or by using docker-compose:

```yaml
---
services:
  sundae:
    image: sundae:dev
    container_name: sundae
    volumes:
      - /path/to/data:/sundae
    ports:
      - 23231:23231
      - 23232:23232
      - 23233:23233
      - 9418:9418
    environment:
      SUNDAE_INITIAL_ADMIN_KEYS: "YOUR_ADMIN_KEY_HERE"
    restart: unless-stopped
```

If you push the image to a registry, use that name instead of `sundae:dev`
(the deployment docs use `ghcr.io/danielboothcloud/sundae:<tag>`).

> **Warning**
>
> Make sure to run the image without a TTY, i.e.: do not use the `--tty`/`-t`
> flags.
