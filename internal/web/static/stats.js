// The statistics view's client half: it draws, and it draws only what the page
// handed it.
//
// Each .plot element carries, in data-series, the JSON the service returned
// (a Timeseries for the rate plot, a GoalSeries for the goal plots), the same
// bytes GET /api/stats/timeseries and GET /api/goals/{id}/progress send. This
// file maps those fields onto axes. It computes no rate, count, streak or
// velocity: the values on a line are the values in the JSON, and the only
// arithmetic is what a chart needs to put a number on a pixel (uPlot does that)
// and to print a fraction as a percent on a tick.
//
// Charts only ever read what the server rendered into the page. They never
// fetch, so there is nothing for a service worker to cache.
document.addEventListener('alpine:init', () => {
  const HEIGHT = { rate: 200, progress: 200, velocity: 120 };

  // A local date as a position on the x axis. Dates are drawn as UTC midnights
  // and labelled in UTC, so a date is never shifted by the browser's own zone.
  const dateX = (d) => Date.parse(d + 'T00:00:00Z') / 1000;
  const instantX = (t) => Date.parse(t) / 1000;
  const pct = (v) => (v == null ? '-' : Math.round(v * 100) + '%');
  const num = (v) => (v == null ? '-' : String(v));

  Alpine.data('chart', () => ({
    plot: null,

    init() {
      this.build();
      this.ro = new ResizeObserver(() => {
        if (this.plot) this.plot.setSize({ width: this.$el.clientWidth, height: HEIGHT[this.$el.dataset.plot] });
      });
      this.ro.observe(this.$el);
      // Colours come from the page's CSS variables, which flip with the theme;
      // rebuilding on a theme change repaints with the other set.
      this.mq = window.matchMedia('(prefers-color-scheme: dark)');
      this.onTheme = () => this.build();
      this.mq.addEventListener('change', this.onTheme);
    },

    // The plot is swapped in and out of the page when the range changes.
    destroy() {
      if (this.ro) this.ro.disconnect();
      if (this.mq) this.mq.removeEventListener('change', this.onTheme);
      if (this.plot) this.plot.destroy();
      this.plot = null;
    },

    build() {
      if (this.plot) { this.plot.destroy(); this.plot = null; }
      const el = this.$el;
      const kind = el.dataset.plot;
      const s = JSON.parse(el.dataset.series);
      const css = getComputedStyle(document.documentElement);
      const c = (n) => css.getPropertyValue(n).trim();

      const view = {
        rate: () => ({
          tz: 'UTC',
          x: s.buckets.map((b) => dateX(b.start)),
          lines: [{ label: s.bucket === 'week' ? 'Week of' : 'Day', y: s.buckets.map((b) => b.completion_rate), color: c('--accent'), fmt: pct }],
          axisFmt: (v) => Math.round(v * 100) + '%',
          range: [0, 1],
        }),
        progress: () => ({
          tz: s.timezone,
          x: s.points.map((p) => instantX(p.at)),
          lines: [
            { label: s.unit === 'percent' ? 'Progress %' : 'Completed', y: s.points.map((p) => p.value), color: c('--accent'), fmt: num },
            { label: 'Target', y: s.points.map(() => s.target), color: c('--muted'), dash: [6, 4], fmt: num, noPoints: true },
          ],
          axisFmt: (v) => String(v),
          range: (u, min, max) => [0, Math.max(max, s.target)],
        }),
        velocity: () => ({
          tz: s.timezone,
          x: s.points.map((p) => instantX(p.at)),
          lines: [{ label: 'Per week', y: s.points.map((p) => p.velocity_per_week), color: c('--snz'), fmt: num }],
          axisFmt: (v) => String(v),
          range: (u, min, max) => [0, max],
        }),
      }[kind]();

      const tzDate = (ts) => uPlot.tzDate(new Date(ts * 1e3), view.tz);
      const axis = (extra) => Object.assign({
        stroke: c('--muted'),
        grid: { stroke: c('--line'), width: 1 },
        ticks: { stroke: c('--line'), width: 1 },
      }, extra);

      const opts = {
        width: el.clientWidth || 320,
        height: HEIGHT[kind],
        tzDate,
        legend: { show: view.lines.length > 1 || kind === 'rate' },
        cursor: { drag: { x: false, y: false } },
        scales: { x: { time: true }, y: { range: view.range } },
        axes: [axis({}), axis({ values: (u, vals) => vals.map(view.axisFmt), size: 44 })],
        series: [
          { label: kind === 'rate' ? 'Date' : 'When', value: kind === 'rate' ? '{YYYY}-{MM}-{DD}' : '{MMM} {D}, {h}:{mm}{aa}' },
          ...view.lines.map((l) => ({
            label: l.label,
            stroke: l.color,
            width: 2,
            dash: l.dash,
            spanGaps: true,
            value: (u, v) => l.fmt(v),
            points: { show: !l.noPoints, size: 8, fill: l.color, stroke: c('--card'), width: 2 },
          })),
        ],
      };
      this.plot = new uPlot(opts, [view.x, ...view.lines.map((l) => l.y)], el);
    },
  }));
});
