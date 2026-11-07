SHELL := /bin/bash
COMPOSE ?= docker compose
HOST ?= $(EXTERNAL_HOST)

.PHONY: build up down restart logs ps status export verify shell clean

build:
	$(COMPOSE) build

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

restart:
	$(COMPOSE) restart

logs:
	$(COMPOSE) logs -f --tail=200

ps:
	$(COMPOSE) ps

status:
	docker exec warp-pool-producer cat /data/status.tsv

export:
	bash ./scripts/export-egress.sh --host "$(HOST)" --out egress-ext.txt

verify:
	bash ./scripts/export-egress.sh --host "$(HOST)" --verify

shell:
	docker exec -it warp-pool-producer bash

clean:
	$(COMPOSE) down -v
