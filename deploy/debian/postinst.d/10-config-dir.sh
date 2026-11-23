#!/bin/bash
# Sourced by the generic postinst after the webitel-svc user is created.
[ -d /etc/webitel/dcctl ] || install -d -m 0750 -o root -g "$GROUP_NAME" /etc/webitel/dcctl
