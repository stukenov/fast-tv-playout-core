# A failed ad request is retried in the background, not turned into a slate for the whole break

Ad pods are asked for in the background (`adBroker`) as soon as a break enters the plan (30 s ahead), and the plan simply stops before a break whose pod is not known yet.
A pod that fails is asked again every second. Only when the break is about to be released (its first clip is due) and there is still no pod, the whole
break becomes slate, decided once, so that a late pod does not start an ad in the middle of it. The call that asks the broker may wait 300 ms for an answer
that is on its way, never longer: a dead or slow ads service must not stop the playlist. See [[the-tick-never-waits-for-the-network]].
