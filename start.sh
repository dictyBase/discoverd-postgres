#!/bin/bash

if [ ${DISCOVERD_PORT_1111_TCP_ADDR+defined} ]; then
    sudo -u postgres DISCOVERD=${DISCOVERD_PORT_1111_TCP_ADDR}:${DISCOVERD_PORT_1111_TCP_PORT} /usr/local/bin/discoverd-postgres $@
else
    sudo -u postgres /usr/local/bin/discoverd-postgres $@
fi

