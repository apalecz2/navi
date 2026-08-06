# User Stories

Single user throughout, so the "as a user" preamble is dropped as noise. Each
story states the intent, then acceptance criteria that are concrete enough to
check by hand. Requirement IDs in brackets.

---

## Epic 1: Creating reminders conversationally

### US-1.1 Create a simple recurring reminder
I message "remind me to take my vitamins every day at 9am" and it exists.

- [ ] The item is created with a `fixed` schedule and the item's timezone [C1]
- [ ] The reply states the interpretation in plain language [A5]
- [ ] The reply lists the next three concrete occurrence timestamps [A5]
- [ ] Occurrences are materialized 30 days forward within the same turn [I3]

### US-1.2 Create a reminder with a random time
I message "remind me to stretch once a day on weekdays, sometime between 9 and 5".

- [ ] A `windowed` schedule is created with the RRULE and the window [C1]
- [ ] Each materialized occurrence has a different, concrete time inside the window [C4]
- [ ] The confirmation shows the specific times chosen, not just the window [A5]

### US-1.3 Create an open-ended reminder without being asked to clarify
I message "remind me to call my grandmother periodically through the week".

- [ ] No clarifying question is asked [A4]
- [ ] "Periodically" resolves via the documented vocabulary table, not model improvisation [A6]
- [ ] A `fuzzy` schedule is created with a count, allowed days, window, and minimum gap [C1, C3]
- [ ] Occurrences are spread across the week rather than clustered [C3]
- [ ] The confirmation names every inferred parameter so I can see what was guessed [A4, A5]

### US-1.4 Correct an interpretation immediately after creating it
After the confirmation I reply "make it more like five times, and not on weekends".

- [ ] The follow-up resolves to the item I just created without me naming it [A9]
- [ ] Only the fields I mentioned change [A9]
- [ ] A fresh confirmation with new timestamps is returned [A5]

### US-1.5 Create a one-off reminder
I message "remind me to renew my passport on the 14th at 10am".

- [ ] A `one_off` schedule is created [C1]
- [ ] Exactly one occurrence exists [C1]
- [ ] The resolved date is validated as being in the future and less than two years out [A11]

### US-1.6 Create a silent reminder
I message "track my morning stretching daily, but don't ping me about it, just check at the end of the day".

- [ ] The item is created with notification policy `silent` [K1]
- [ ] Occurrences are generated and visible in the day view and calendar [K2]
- [ ] No push fires at the scheduled time [K1]
- [ ] If unresolved, the item appears in that evening's reconciliation message [K3, K5]

---

## Epic 2: Modifying and removing reminders

### US-2.1 Change a schedule for all future occurrences
I message "move my vitamins to 8pm".

- [ ] All future `pending` occurrences are deleted and re-materialized [Edit scope: `future_all`]
- [ ] Occurrences already `notified`, `completed`, `skipped`, `snoozed`, or `missed` are untouched [Principle 2]
- [ ] A pending occurrence later today is regenerated at the new time
- [ ] If the new time has already passed today, the change starts tomorrow

### US-2.2 Skip one instance without changing the schedule
I message "skip tomorrow's gym reminder".

- [ ] Exactly one occurrence is affected [Edit scope: `single`]
- [ ] That occurrence is flagged as an override
- [ ] The next materialization run does not recreate or overwrite it
- [ ] The recurring schedule is unchanged

### US-2.3 Change a schedule starting from a date
I message "starting in September, make the run reminder three times a week".

- [ ] Occurrences before the boundary date are left alone [Edit scope: `from_date`]
- [ ] Occurrences after it are regenerated under the new schedule

### US-2.4 Rename without disturbing the calendar
I message "call the vitamins one 'supplements'".

- [ ] Only the title changes
- [ ] No re-materialization occurs, because no schedule field changed
- [ ] The calendar view is unaffected

### US-2.5 Delete a reminder
I message "delete the gym reminder".

- [ ] The agent asks for confirmation before deleting [A7]
- [ ] The item is identified from injected context without a lookup round-trip [A3]
- [ ] Historical resolved occurrences are retained for statistics [Principle 2]

### US-2.6 Pause everything while away
I message "I'm away until Monday, pause everything".

- [ ] A suspension window is set rather than eighteen individual skips [I6]
- [ ] The materializer skips the paused range
- [ ] No notifications and no reconciliation messages fire during the window
- [ ] Normal operation resumes automatically afterwards

---

## Epic 3: Receiving reminders

### US-3.1 Receive a reminder on my phone
A reminder comes due.

- [ ] A push arrives on iPhone within one minute of the scheduled time [Q1]
- [ ] The body is the pre-generated text from the occurrence row [N2]
- [ ] If no generated text exists, the plain title is sent instead [N3]
- [ ] The occurrence moves to `notified` [R1]

### US-3.2 Resolve from the notification without opening an app
I long-press the notification on my lock screen.

- [ ] Done, Snooze, and Skip appear as native notification actions [N4, T6]
- [ ] Tapping one resolves the occurrence via an HTTP request from the notification [N4]
- [ ] A short confirmation push follows, because the original notification does not self-dismiss [N6]
- [ ] Tapping twice does not double-record [R4]

### US-3.3 Snooze
I tap Snooze on a reminder.

- [ ] The original occurrence becomes `snoozed` and keeps its true scheduled time [R6]
- [ ] A child occurrence is created with an incremented snooze depth [R6]
- [ ] The child is flagged as an override so materialization leaves it alone [R6]
- [ ] Completing the child counts the whole chain as one completion, and the streak survives [R7]
- [ ] Past the depth cap, the chain resolves as `missed` [R8]

### US-3.4 Snooze until a natural time
I tap "tonight".

- [ ] The delta resolves against the item's timezone and window, not naive arithmetic [R9]

### US-3.5 Reminder priority breaks through a Focus mode
A high-priority reminder fires while my phone is in Focus.

- [ ] Priority maps to the iOS interruption level [N5]
- [ ] Low-priority reminders do not break through

---

## Epic 4: Resolving reminders

### US-4.1 Report completion before the reminder fires
At 7am I message "did my stretching already", for a reminder due at 6pm.

- [ ] Today's occurrence is resolved as `completed` whether `pending` or `notified` [R3]
- [ ] The pending notification is cancelled and does not fire later [R3]
- [ ] The completion feeds the copywriter's context for subsequent occurrences [G2]

### US-4.2 Report several completions in one plain-text message
I message "did stretching, vitamins, and the walk".

- [ ] All three resolve in a single atomic operation [A8]
- [ ] Fuzzy name matching maps my phrasing to the right items [A8]
- [ ] The reply summarises counts rather than listing three separate confirmations [A8]
- [ ] If one name is ambiguous, nothing is written and the agent asks about that one only [A11]

### US-4.3 Report completions with an exception
I message "did everything today except the walk, I was travelling".

- [ ] "Everything" resolves against today's outstanding occurrences from injected context [A3, A8]
- [ ] The walk is recorded as `skipped`, not `missed` [R2]
- [ ] "I was travelling" is stored as the resolution note [R2]
- [ ] The skip does not break the streak the way a miss would [R2]
- [ ] The skip reason is available to the copywriter, so the next message does not scold [G2, G8]

### US-4.4 Check items off in a web view
I open the app on my phone.

- [ ] Everything due today is listed with current status [V1]
- [ ] One tap resolves an item [V1]
- [ ] The UI updates immediately without waiting for the server [V3]
- [ ] The same endpoints back this view as back the notification buttons [V2]
- [ ] The view is installable to the home screen [V1]

---

## Epic 5: Reconciliation

### US-5.1 Get one end-of-day check-in
It is 21:00 and three items are unresolved.

- [ ] A single message covers all three [K4]
- [ ] It includes both silent items and notified-but-ignored items [K5]
- [ ] Nothing has yet been marked `missed` [K6]

### US-5.2 Answer the check-in naturally
I reply "stretching and vitamins yes, skipped the walk".

- [ ] All three resolve from the one reply [A8]
- [ ] The reply is understood as a response to the check-in, not a new request [A9]

### US-5.3 Ignore the check-in
I do not reply.

- [ ] After the grace window the unresolved items become `missed` [K6]
- [ ] Grace defaults to end of local day and is overridable per item [K7]
- [ ] Misses feed streak and completion-rate statistics [V5]

### US-5.4 Per-item reconciliation timing
A reminder should be checked at 14:00 rather than 21:00.

- [ ] Reconciliation time is overridable per item [K8]

---

## Epic 6: Personality and proactivity

### US-6.1 Varied, context-aware reminder text
I receive the same daily reminder for two weeks.

- [ ] Wording differs each day [G6]
- [ ] Recent phrasings are passed in to prevent loops [G6]
- [ ] Voice matches `persona.md` and can be retuned without a rebuild [G5]
- [ ] It never guilts or scolds [G8]

### US-6.2 Tone reflects how I am actually doing
- [ ] On a streak, the streak is named and framed as worth protecting [G7]
- [ ] After a single miss, the miss is not mentioned [G7]
- [ ] After repeated misses, the ask shrinks rather than the volume rising [G7]

### US-6.3 The agent questions a schedule that is not working
An item has been missed five times running, or snoozed three times in a day.

- [ ] The agent asks whether to reschedule, shrink, or drop it, rather than nagging again [G7, R10]
- [ ] It proposes a specific alternative [A12]
- [ ] It does not apply the change without my agreement [A12]

### US-6.4 Proactivity stays bounded
- [ ] Unprompted messages are capped per day [A13]
- [ ] Proactive triggers are deterministic conditions in code, not model whim [Principle 3]

---

## Epic 7: Dashboards

### US-7.1 See what is coming
- [ ] A calendar view shows upcoming occurrences by date [V4]
- [ ] Random and fuzzy schedules show their concrete resolved times [C4]
- [ ] Colour distinguishes items, and status distinguishes pending from resolved [V4]
- [ ] The view is one date-range query with no client-side recurrence expansion [V4]

### US-7.2 See how I am doing
- [ ] Completion rate over time [V5]
- [ ] Current and longest streak per item [V5]
- [ ] Median lag from notification to resolution [V5]
- [ ] Time-of-day completion heatmap [V5]
- [ ] Snooze chains count once, not once per link [R7]

### US-7.3 Ask the agent for the same numbers
I message "how am I doing on stretching this month?"

- [ ] The answer comes from the same aggregation code as the dashboard [V6]

---

## Epic 8: Calendar integration

### US-8.1 See reminders in my phone's calendar
- [ ] An authenticated `.ics` URL is subscribable from Google Calendar and Apple Calendar [X1]
- [ ] `fixed` and `windowed` items appear as recurring events [X2]
- [ ] `fuzzy` items appear as individual events at their resolved times [X2]

### US-8.2 Manage calendar events the same way as reminders (later)
- [ ] Events are an item kind with duration [S4, X4]
- [ ] The same conversational tools create, modify, and delete them [X4]
- [ ] Events use an occurrence status set without completion semantics [R1]

---

## Epic 9: Operations

### US-9.1 Survive a restart
The container restarts while reminders are pending.

- [ ] Occurrences overdue by less than the threshold fire immediately [C9]
- [ ] Older ones pass to reconciliation instead of firing a backlog [C9]
- [ ] Nothing fires twice [Q3]

### US-9.2 Survive a model outage
The model provider is unavailable.

- [ ] Reminders still fire, using plain titles [N3, Q4]
- [ ] Generation attempts are bounded rather than retried indefinitely [G4]
- [ ] The failure is visible in the call log [L6]

### US-9.3 Recover from host loss
- [ ] Restoring the Litestream replica and starting the container recovers the system [Q5]

### US-9.4 Swap the messaging channel
I decide to move conversation from Telegram to Discord.

- [ ] Only a new adapter and a configuration change are required [T2, T8]
- [ ] No scheduler, agent, or resolution code changes [T5]
