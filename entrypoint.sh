#!/bin/bash
set -e

# Start Certbot auto-renewal in the background
certbot renew --quiet --no-random-sleep-on-renew &

# Start Nginx
nginx &

sleep 2

# Start the reprox Go application
/usr/bin/reprox

