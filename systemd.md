# Running Sundae as a Systemd Service

Most Linux OSes use Systemd as an init system and service management. You can
use Systemd to manage Sundae as a service on your host machine.

Build the `sundae` binary first (see the
[installation instructions](README.md#installation)) and install it as
`/usr/bin/sundae`. The unit, sysusers and tmpfiles snippets in
[`.nfpm/`](.nfpm) are the same setup with a dedicated `sundae` user.

## Writing a Systemd Service File

Start by writing a Systemd service file to define how your Sundae server
should start.

First, we need to specify where the data should live for our server. Here I
will be choosing `/var/local/lib/sundae` to store the server's data. Sundae
will look for this path in the `SUNDAE_DATA_PATH` environment
variable.

Make sure this directory exists before proceeding.

```sh
sudo mkdir -p /var/local/lib/sundae
```

We will also create a `/etc/sundae.conf` file for any extra server settings that we want to override.

```conf
# Config defined here will override the config in /var/local/lib/sundae/config.yaml
# Keys defined in `SUNDAE_INITIAL_ADMIN_KEYS` will be merged with
# the `initial_admin_keys` from /var/local/lib/sundae/config.yaml.
#
#SUNDAE_GIT_LISTEN_ADDR=:9418
#SUNDAE_HTTP_LISTEN_ADDR=:23232
#SUNDAE_SSH_LISTEN_ADDR=:23231
#SUNDAE_SSH_KEY_PATH=ssh/sundae_host_ed25519
#SUNDAE_INITIAL_ADMIN_KEYS='ssh-ed25519 AAAAC3NzaC1lZDI1...'
```

> **Note** Sundae stores its server configuration and settings in
> `config.yaml` under its _data path_ directory specified using
> `SUNDAE_DATA_PATH` environment variable.

Now, let's write a new `/etc/systemd/system/sundae.service` Systemd service file:

```conf
[Unit]
Description=Sundae git server 🍦
Documentation=https://github.com/danielboothcloud/sundae
Requires=network-online.target
After=network-online.target

[Service]
Type=simple
Restart=always
RestartSec=1
ExecStart=/usr/bin/sundae serve
Environment=SUNDAE_DATA_PATH=/var/local/lib/sundae
EnvironmentFile=-/etc/sundae.conf
WorkingDirectory=/var/local/lib/sundae

[Install]
WantedBy=multi-user.target
```

Great, we now have a Systemd service file for Sundae. The settings defined
here may vary depending on your specific setup. This assumes that you want to
run Sundae as `root`. For more information on Systemd service files, refer
to
[systemd.service](https://www.freedesktop.org/software/systemd/man/systemd.service.html)

## Start Sundae on boot

Now that we have our Sundae Systemd service file in-place, let's go ahead
and enable and start Sundae to run on-boot.

```sh
# Reload systemd daemon
sudo systemctl daemon-reload
# Enable Sundae to start on-boot
sudo systemctl enable sundae.service
# Start Sundae now!!
sudo systemctl start sundae.service
```

You can monitor the server logs using `journalctl -u sundae.service`. Use
`-f` to _tail_ and follow the logs as they get written.

