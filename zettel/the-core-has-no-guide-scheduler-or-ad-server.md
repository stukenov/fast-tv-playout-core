# The core plays and takes commands; guide, scheduler and ad server are somebody else's

The repository had scheduler (fills the schedule), epg (Now/Next, XMLTV), ads (picks creatives) and Redis for their caches. Every one of them is a product decision
(what to air, how to sell it, what the guide looks like) that differs for each user, and every one needed its own tests, keys and metrics. We cut them and kept
a seam for each: the schedule API for the scheduler, `now-next` and the events for the guide, the ads webhook (or a registry file) for the ad server.
What stays is what only the engine can do: put sources on one timeline and say what is on the air. Reading the registry file is the one piece of ads left in,
because without it a user needs an ad server before the first break airs ([[playout-waits-for-the-ad-server]]).
