# The engine's loop never waits for the network, because a short playlist has no buffer to hide it

With a window of 180 segments a stalled step is invisible. With a window of two segments (the one on the air and the next one) the player has about one second of
buffer, so every blocking call in the loop (an ad request of 2 s, a source segment of 3 s, a database read without a timeout) would freeze the picture.
So: ad pods are fetched in the background and the plan waits for them; source segments are downloaded ahead (the next 10 s) into a memory cache and the slate is
kept there permanently; every wait that is left has a budget (half the lead, 300 ms at the least); the schedule is read at most twice a second with a 700 ms timeout and
the last schedule goes on airing through an outage; a step stops releasing clips after a second. A clip that cannot be made is tried once.
The tests slow and break each of these on purpose and measure how long a step takes; each safeguard was removed once to see that a test fails. See [[one-engine-instead-of-playout-and-packager]].
