# "Ad break now" is an edit of the schedule, not a message to the engine

The control API could send the engine a live command. We write the change into the schedule instead (cut the entry that is on, insert the break, shift what follows,
the cut entry resumes by `offset_s`) and the engine finds it on its next tick. The engine stays a pure function of the schedule and the clock, a restart or a second
engine needs nothing else, and the same edit is idempotent and checked by `If-Match` like any other. The price is `CUE_LEAD_S` plus `LIVE_LEAD_S` between the call
and the air: a clip that is already released into the playlist cannot change ([[one-engine-instead-of-playout-and-packager]]). The edit runs under `SELECT ... FOR UPDATE`
on the channel: two commands at once used to cut the same entry twice.
