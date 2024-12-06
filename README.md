# reprox

Use `reprox.host` Docker container label to specify the host to proxy requests to.  
Set `CERTBOT_EMAIL` environment variable to specify the email to use for Let's Encrypt certificate generation.  
Mount `/var/run/docker.sock` to enable docker container discovery.
