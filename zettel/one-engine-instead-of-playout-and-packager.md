# Playout and packager are one process, and a clip is final only when it is released

They were two services with a Redis stream between them: playout planned and published clips 30 s ahead, the packager read them. The stream added
Redis to the core, a restart protocol (duplicates, old clips), and a rule that a published clip is never revised, so a schedule change needed 40 s notice.
In one process the plan is rebuilt on every step from the schedule and the clock, and only the clips whose time has come (LIVE_LEAD_S before they air) go into the
playlist. Until then they can change, so a change reaches the air a second or two later in the `tight` profile. A restart needs no stream: the engine restores the
window from the playlist file and the timeline comes from the clock ([[decode-times-follow-the-clock]]).
Redis is gone from the whole core: the scheduler, epg and ads services were cut as well ([[the-core-has-no-guide-scheduler-or-ad-server]]).
Not done: several channels in one process (the planning state is per engine).
