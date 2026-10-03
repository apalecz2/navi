// The day view's client half: the optimistic flip (V3) and nothing else.
//
// This file decides no outcome. A tap flips the row, POSTs to the same /api
// endpoint the Telegram button and the agent reach, and then throws the guess
// away and swaps in what the server renders. A 200, a 200 no-op and a 409 are
// therefore the same path to the same place - the true current state - and the
// transition table stays in one language on one side of the wire.
document.addEventListener('alpine:init', () => {
  Alpine.data('day', () => ({
    labels: {},
    inflight: 0,
    reached: false,
    touched: [],
    offline: !navigator.onLine,

    init() {
      this.labels = JSON.parse(this.$el.dataset.labels || '{}');
      const refresh = () => { if (!document.hidden && this.inflight === 0) this.settle(); };
      document.addEventListener('visibilitychange', refresh);
      window.addEventListener('online', () => { this.offline = false; refresh(); });
      window.addEventListener('offline', () => { this.offline = true; });
    },

    // row is the row's own Alpine scope ($data), so flipping row.s is the
    // instant part: no request has been made when the badge changes.
    async act(row, id, action, arg) {
      const prev = row.s;
      const resolving = action === 'snooze' ? 'snoozed' : action === 'skip' ? 'skipped' : 'completed';
      const path = action === 'snooze' ? 'snooze' : 'resolve';
      const body = action === 'snooze'
        ? { delta: arg, source: 'web' }
        : { status: resolving, note: null, source: 'web' };

      row.err = '';
      row.menu = false;
      row.s = resolving;
      this.inflight++;
      this.touched.push(row);

      try {
        // Any HTTP answer counts as reaching the server. The status code is not
        // read: what the row should show is the server's call, fetched below.
        await fetch('/api/occurrences/' + id + '/' + path, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
          credentials: 'same-origin',
          cache: 'no-store',
        });
        this.reached = true;
      } catch (e) {
        // Never arrived, so the guess has nothing behind it. Put the row back.
        // Retrying is safe: a request that did land is the idempotency table's
        // second row.
        row.s = prev;
        row.err = 'Not sent. Check your connection and tap again.';
        this.touched = this.touched.filter(r => r !== row);
      } finally {
        this.inflight--;
      }
      if (this.inflight === 0 && this.reached) await this.settle();
    },

    // settle replaces the list with the server's rendering. It runs once, when
    // the last tap in a burst has landed, so a slow first response cannot wipe
    // a second row's flip with a list rendered before its request arrived.
    async settle() {
      const touched = this.touched;
      this.touched = [];
      this.reached = false;
      try {
        const res = await fetch('/app/today', { credentials: 'same-origin', cache: 'no-store' });
        if (!res.ok) throw new Error('status ' + res.status);
        htmx.swap('#day-body', await res.text(), { swapStyle: 'innerHTML' });
        this.offline = false;
      } catch (e) {
        // Sent, but the answer could not be read. The row is showing a guess;
        // say so rather than let it pass for the server's word.
        touched.forEach(r => { r.err = 'Sent, not confirmed. Refresh to check.'; });
      }
    },
  }));
});
