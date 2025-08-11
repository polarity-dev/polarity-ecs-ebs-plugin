# Polarity EBS plugin for ECS
The docker plugin is not a regular docker container. It is just a folder with a `config.json` and a `rootfs`:
- `config.json` is the file that describes the plugin, where to find the binary of the plugin and what paths to mount
- `rootfs` is the isolated filesystem of the plugin, to comunicate with the host machine we need to mount the path that we want to work on (`/dev`)
- our binary will be located in `/rootfs/bin`

## Building the plugin
The plugin needs to be compiled for intel and arm architecture separately and to work it needs some other tools and files.
The plugin will run on an ec2 and it will use the aws sdk, this sdk when running on an ec2 will use the iam role of the host machine but this will work only if the host machine has the necessary certificates to call the aws api.
But the plugin has a completely separate filesystem so it can't access the certificates on the host machine, we have also the same issue when trying to use other binaries like `lsblk` or `mkfs.xfs`
To avoid this errors we will build the plugin with docker, we install the certificates and the other tools and then we export the fs of the docker image and so we have the complete plugin working.

