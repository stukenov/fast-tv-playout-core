# An event is published when it goes on the air, not when it is planned

The engine plans 30 s ahead and releases clips `LIVE_LEAD_S` before they air, so "the break started" is known long before the viewer sees it. A subscriber that
switches a graphic or a counter on that event would be early by that lead. `announce` therefore waits until the clock reaches the start of the clip. Events
are kept in a ring of 1000 for SSE (`Last-Event-ID`) and sent to webhooks from a queue of 512 per receiver that drops the oldest: a dead receiver may lose events, it may never slow the engine
([[the-tick-never-waits-for-the-network]]). `engine.started` is published before any receiver is reachable, so it is only guaranteed over SSE.
