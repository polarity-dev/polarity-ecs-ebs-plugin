# Polarity EBS plugin for ECS

## How does this work
Here is an example of the plugin working by calling the docker cli for simplicity
```
docker volume create -d polarity-ecs-ebs-plugin <ebs-volume-id>
docker run --rm -it -v <ebs-volume-id>:/data alpine
```
When the container is started we call AWS to get `<ebs-volume-id>` informations, the volume is eventually detached from other EC2 and attached to the cluster.
Then if the volume has no filesystem, it will be created using `mkfs.xfs`.
Then the volume will be mounted in a location managed by docker and accessible from the mountpoint in the container.

The volume needs to be in the same az as the EC2

Here is an example of the plugin working with CloudFormation
```yml
  TaskDefinition:
    Type: AWS::ECS::TaskDefinition
    Properties:
      ContainerDefinitions:
        MountPoints:
          - SourceVolume: <ebs-volume-id>
            ContainerPath: <your desired path of your app in the container>
      Volumes:
        - Name: <ebs-volume-id>
          DockerVolumeConfiguration:
            Scope: shared
            Autoprovision: true
            Driver: polarity-ecs-ebs-plugin
            Labels:
              Name: <ebs-volume-id>

```
When the task is created the volume will be attached to the task.

In this case the plugin should already be installed in the host machine.
This can be done either using a custom AMI or in the EC2 user data.


## Installation
Firstrly pick the correct release based on your system.

Install the plugin from `.tar.gz` release
```sh
curl -o polarity-ecs-ebs-plugin.tar.gz https://github.com/polarity-dev/polarity-ecs-ebs-plugin/releases/download/<release_tag>/polarity-ecs-ebs-plugin.amd64.tar.gz # aws s3 cp <source> polarity-ecs-ebs-plugin.tar.gz
mkdir polarity-ecs-ebs-plugin
tar -xzf polarity-ecs-ebs-plugin.tar.gz -C polarity-ecs-ebs-plugin
docker plugin create polarity-ecs-ebs-plugin ./polarity-ecs-ebs-plugin
docker plugin enable polarity-ecs-ebs-plugin
```

NOTE: If you are installing the plugin on the ec2 that hosts the ecs cluster remember to restart the `ecs` service with `systemctl`

## Logging
The plugin logs on docker journalctl
```sh
journalctl -u docker
```

## Building the plugin
Docker plugins are not regular docker containers. They are just a folder with a `config.json` and a `rootfs`:
- `config.json` is the file that describes the plugin, where to find the binary of the plugin and what paths to mount
- `rootfs` is the isolated filesystem of the plugin, to comunicate with the host machine we need to mount the path that we want to work on (`/dev`)
- our binary will be located in `/rootfs/bin`

The plugin needs to be compiled for intel and arm architecture separately and to work it needs some other tools and files.
The plugin will run on an ec2 and it will use the aws sdk, this sdk when running on an ec2 will use the iam role of the host machine but this will work only if the host machine has the necessary certificates to call the aws api.
But the plugin has a completely separate filesystem so it can't access the certificates on the host machine, we have also the same issue when trying to use other binaries like `lsblk` or `mkfs.xfs`
To avoid this errors we will build the plugin with docker, we install the certificates and the other tools and then we export the fs of the docker image and now we have the complete plugin working.

To develop on the plugin you can run
```sh
make dev
```
This will start a local sock with the server
You can also run `make health-check` to check if the server is responding

To test the full functionality of the plugin you should run `make debug-tar-amd64` and copy the `.tar.gz` file on your ecs cluster
This version will also create a log file in `/var/log/polarity-ecs-ebs.log`

To call manually the server on ecs cluster you should ssh into the cluster and then follow the installation guide.

Now your plugin will be enabled, the sock file will be located in `/var/run/docker/plugins/` in a folder with the plugin hash.
You just need to run something like this
```sh
curl -H "Content-Type: application/json" -XPOST -d '{ "Name": "test" }' --unix-socket ./pl-ebs.sock http:/localhost/health
```
