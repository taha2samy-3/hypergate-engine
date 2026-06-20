#!/usr/bin/env bash
openssl req -x509 -newkey rsa:2048 -keyout tls.key -out tls.crt -days 365 -nodes -subj "/CN=hyper-operator.hyper-system.svc" -addext "subjectAltName=DNS:hyper-operator.hyper-system.svc"
kubectl create secret tls webhook-server-cert --cert=tls.crt --key=tls.key -n hyper-system --dry-run=client -o yaml | kubectl apply -f - && rm tls.key tls.crt
