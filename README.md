# PulseLB

A load balancer written from scratch in Go that keeps a site up during a
"Tatkal 10 AM rush": it reroutes around slow servers, heals itself when one
dies, protects critical traffic under overload, and proves it in a live race
against a classic round-robin balancer.

Work in progress. The full README is written as each feature lands.
