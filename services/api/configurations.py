#!/usr/bin/env python
# -*- coding: utf-8 -*-

# This file is part of Flower.
#
# Copyright ©2018 Nicolò Mazzucato
# Copyright ©2018 Antonio Groza
# Copyright ©2018 Brunello Simone
# Copyright ©2018 Alessio Marotta
# DO NOT ALTER OR REMOVE COPYRIGHT NOTICES OR THIS FILE HEADER.
#
# Flower is free software: you can redistribute it and/or modify
# it under the terms of the GNU General Public License as published by
# the Free Software Foundation, either version 3 of the License, or
# (at your option) any later version.
#
# Flower is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with Flower.  If not, see <https://www.gnu.org/licenses/>.

import os
from pathlib import Path

traffic_dir = Path(os.getenv("PETAL_TRAFFIC_DIR", "/traffic"))
dump_pcaps_dir = Path(os.getenv("DUMP_PCAPS", "/traffic"))
tick_length = os.getenv("TICK_LENGTH", 2*60*1000)
flag_lifetime = os.getenv("FLAG_LIFETIME", 5)
start_date = os.getenv("TICK_START", "2018-06-27T13:00:00+02:00")
flag_regex = os.getenv("FLAG_REGEX", "[A-Z0-9]{31}=")
vm_ip = os.getenv("VM_IP", "10.10.3.1")
visualizer_url = os.getenv("VISUALIZER_URL", "http://127.0.0.1:1337")

# Game services shown in the UI, from the SERVICES env var:
#   SERVICES="welcome:7001,cybercorp:7002"       (name:port, ip defaults to VM_IP)
#   SERVICES="web:10.60.2.1:80,pwn:10.60.2.1:1337" (name:ip:port)
def _parse_services(raw):
    out = []
    for entry in raw.replace("\n", ",").split(","):
        parts = [p.strip() for p in entry.strip().split(":")]
        if len(parts) == 2 and parts[0] and parts[1].isdigit():
            out.append({"ip": vm_ip, "port": int(parts[1]), "name": parts[0]})
        elif len(parts) == 3 and parts[0] and parts[2].isdigit():
            out.append({"ip": parts[1], "port": int(parts[2]), "name": parts[0]})
        elif entry.strip():
            print(f"[configurations] ignoring malformed SERVICES entry: {entry!r}")
    return out

services = _parse_services(os.getenv("SERVICES", ""))
services += [{"ip": vm_ip, "port": -1, "name": "other"}]
