## Build and install Intel CPU pool webhook for admission controller

### Get source code

    $ mkdir -p $GOPATH/src/github.com/intel/
    $ cd $GOPATH/src/github.com/intel/
    $ git clone https://github.com/intel/intel-device-plugins-for-kubernetes.git

### Build a Docker image with the webhook

    $ export SRC=$GOPATH/src/github.com/intel/intel-device-plugins-for-kubernetes
    $ cd $SRC
    $ make intel-cpu-pool-admissionwebhook
    $ docker images
    REPOSITORY                    TAG                                        IMAGE ID            CREATED          SIZE
    intel-cpu-pool-admissionwebhook   10efe163a5091e8b2ceaa9baad236d3a41063c88   6c3bce0b8693        0 sec ago        25.2MB
    intel-cpu-pool-admissionwebhook   devel                                      6c3bce0b8693        0 sec ago        25.2MB
    ...

### Deploy webhook service

Make sure you have `cfssl` and `jq` utilities installed on your host.
Then run the script `scripts/cpu-pool-webhook-deploy.sh`.

    $ cd $SRC
    $ ./scripts/cpu-pool-webhook-deploy.sh
    Create secret including signed key/cert pair for the webhook
    Creating certs in /tmp/tmp.JYgcFiaoCZ
    certificatesigningrequest "intel-cpu-pool-webhook-svc.default" created
    NAME                             AGE       REQUESTOR      CONDITION
    intel-cpu-pool-webhook-svc.default   1s        system:admin   Pending
    certificatesigningrequest "intel-cpu-pool-webhook-svc.default" approved
    secret "intel-cpu-pool-webhook-certs" created
    Removing /tmp/tmp.JYgcFiaoCZ
    Create webhook deployment
    deployment "intel-cpu-pool-webhook-deployment" created
    Create webhook service
    service "intel-cpu-pool-webhook-svc" created
    Register webhook
    mutatingwebhookconfiguration "cpu-pool-mutator-webhook-cfg" created

Please note that the script needs the CA bundle used for signing cerificate
requests in your cluster. By default it fetches the bundle stored
in the configmap `extension-apiserver-authentication`. But it may differ from
the actual signing cerificate which is passed in the option
`--cluster-signing-cert-file` to `kube-controller-manager`. In this case
you need to point the script to the actual signing cerificate:

    $ ./scripts/webhook-deploy.sh --ca-bundle-path /var/run/kubernetes/server-ca.crt
