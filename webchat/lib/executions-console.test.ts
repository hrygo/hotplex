import { describe, it, expect } from 'vitest';

import {
  EMPTY_EXECUTION_PAGE,
  EMPTY_EXECUTION_FILTERS,
  ACTION_CONFIRM_KEYS,
  ACTION_LABEL_KEYS,
  NOTE_LABEL_KEYS,
  PHASE_LABEL_KEYS,
  STATUS_LABEL_KEYS,
  actionConfirmKey,
  actionLabelKey,
  deliveryTone,
  evidenceLabelKey,
  evidenceTone,
  filtersAreClean,
  isTerminalRuntime,
  mergeExecutionPage,
  noteLabelKey,
  phaseLabelKey,
  phaseRank,
  runtimeTone,
  sortTimelineItems,
  statusLabelKey,
  truncateMiddle,
} from './executions-console';
import { actionRequest } from './api/admin-executions';
import type {
  AdminAvailableAction,
  AdminExecutionSummary,
  AdminTimelineItem,
} from './types/admin';

function item(overrides: Partial<AdminTimelineItem>): AdminTimelineItem {
  return {
    phase: 'running',
    source: 'event_store',
    kind: 'turn.start',
    fact_time: 1000,
    observed_at: 2000,
    ...overrides,
  };
}

function summary(executionId: string, overrides: Partial<AdminExecutionSummary> = {}): AdminExecutionSummary {
  return {
    execution_id: executionId,
    session_id: 'sess-1',
    delivery_status: 'delivered',
    runtime_status: 'completed',
    fence_version: 0,
    created_at: 1000,
    updated_at: 2000,
    ...overrides,
  };
}

describe('phaseRank', () => {
  it('orders phases along the life of a run', () => {
    expect(phaseRank('accepted')).toBeLessThan(phaseRank('queued'));
    expect(phaseRank('queued')).toBeLessThan(phaseRank('dispatched'));
    expect(phaseRank('dispatched')).toBeLessThan(phaseRank('running'));
    expect(phaseRank('running')).toBeLessThan(phaseRank('delivery'));
    expect(phaseRank('delivery')).toBeLessThan(phaseRank('terminal'));
    expect(phaseRank('terminal')).toBeLessThan(phaseRank('fenced'));
  });

  it('sorts an unknown phase last instead of dropping it', () => {
    expect(phaseRank('something_new')).toBeGreaterThan(phaseRank('truncated'));
  });
});

describe('sortTimelineItems', () => {
  it('groups mixed sources by phase, then by fact time', () => {
    const sorted = sortTimelineItems([
      item({ phase: 'terminal', fact_time: 400 }),
      item({ phase: 'accepted', fact_time: 100 }),
      item({ phase: 'running', fact_time: 300 }),
      item({ phase: 'accepted', fact_time: 200 }),
    ]);
    expect(sorted.map((i) => [i.phase, i.fact_time])).toEqual([
      ['accepted', 100],
      ['accepted', 200],
      ['running', 300],
      ['terminal', 400],
    ]);
  });

  it('is stable for facts sharing a phase and instant', () => {
    const a = item({ kind: 'a', source: 'event_store' });
    const b = item({ kind: 'b', source: 'execution_store' });
    const sorted = sortTimelineItems([a, b]);
    // Different sources sort deterministically by name, and the relative order
    // does not depend on the input order.
    expect(sorted.map((i) => i.kind)).toEqual(['a', 'b']);
    expect(sortTimelineItems([b, a]).map((i) => i.kind)).toEqual(['a', 'b']);
  });

  it('keeps a truncation marker at the end', () => {
    const sorted = sortTimelineItems([
      item({ phase: 'truncated', kind: 'window_truncated', fact_time: 0 }),
      item({ phase: 'accepted', fact_time: 100 }),
    ]);
    expect(sorted[0].phase).toBe('accepted');
    expect(sorted[sorted.length - 1].phase).toBe('truncated');
  });

  it('does not mutate its input', () => {
    const input = [item({ phase: 'terminal' }), item({ phase: 'accepted' })];
    const copy = [...input];
    sortTimelineItems(input);
    expect(input).toEqual(copy);
  });
});

describe('mergeExecutionPage', () => {
  it('appends a new page and keeps the cursor', () => {
    const first = mergeExecutionPage(EMPTY_EXECUTION_PAGE, [summary('a'), summary('b')], {
      before_created_at: 1000,
      before_execution_id: 'b',
    });
    expect(first.rows.map((r) => r.execution_id)).toEqual(['a', 'b']);
    expect(first.cursor).toEqual({ before_created_at: 1000, before_execution_id: 'b' });

    const second = mergeExecutionPage(first, [summary('c')], null);
    expect(second.rows.map((r) => r.execution_id)).toEqual(['a', 'b', 'c']);
    expect(second.cursor).toBeNull();
  });

  it('drops a row the operator is already looking at', () => {
    const first = mergeExecutionPage(EMPTY_EXECUTION_PAGE, [summary('a'), summary('b')], null);
    // A run created between the two requests can shift the keyset window and
    // hand back 'b' again.
    const second = mergeExecutionPage(first, [summary('b'), summary('c')], null);
    expect(second.rows.map((r) => r.execution_id)).toEqual(['a', 'b', 'c']);
  });

  it('does not mutate the previous page', () => {
    const first = mergeExecutionPage(EMPTY_EXECUTION_PAGE, [summary('a')], null);
    const rowsBefore = first.rows.length;
    mergeExecutionPage(first, [summary('b')], null);
    expect(first.rows).toHaveLength(rowsBefore);
  });
});

describe('filtersAreClean', () => {
  it('is true only for an unfiltered view', () => {
    expect(filtersAreClean(EMPTY_EXECUTION_FILTERS)).toBe(true);
    expect(filtersAreClean({ ...EMPTY_EXECUTION_FILTERS, sessionId: '  ' })).toBe(true);
  });

  it('is false when any filter narrows the set', () => {
    expect(filtersAreClean({ ...EMPTY_EXECUTION_FILTERS, sessionId: 'sess-1' })).toBe(false);
    expect(filtersAreClean({ ...EMPTY_EXECUTION_FILTERS, deliveryStatus: 'unknown' })).toBe(false);
    expect(filtersAreClean({ ...EMPTY_EXECUTION_FILTERS, runtimeStatus: 'failed' })).toBe(false);
    expect(filtersAreClean({ ...EMPTY_EXECUTION_FILTERS, sinceMs: 1 })).toBe(false);
    expect(filtersAreClean({ ...EMPTY_EXECUTION_FILTERS, untilMs: 1 })).toBe(false);
  });
});

describe('isTerminalRuntime', () => {
  it('treats unknown as terminal — it is the end of the recorded state machine', () => {
    expect(isTerminalRuntime('completed')).toBe(true);
    expect(isTerminalRuntime('failed')).toBe(true);
    expect(isTerminalRuntime('unknown')).toBe(true);
  });

  it('does not treat in-flight states as terminal', () => {
    expect(isTerminalRuntime('queued')).toBe(false);
    expect(isTerminalRuntime('pending')).toBe(false);
    expect(isTerminalRuntime('running')).toBe(false);
    expect(isTerminalRuntime('')).toBe(false);
  });
});

describe('evidence labelling', () => {
  it('distinguishes recorded, never recorded and unavailable', () => {
    expect(evidenceLabelKey('recorded')).toBe('executions.evidence.recorded');
    expect(evidenceLabelKey('not_recorded_for_this_run')).toBe('executions.evidence.not_recorded');
    expect(evidenceLabelKey('unavailable')).toBe('executions.evidence.unavailable');
  });

  it('never presents missing history as recorded', () => {
    expect(evidenceTone('recorded')).toBe('ok');
    expect(evidenceTone('not_recorded_for_this_run')).toBe('warn');
    expect(evidenceTone('unavailable')).toBe('muted');
    expect(evidenceTone(undefined)).toBe('muted');
    expect(evidenceLabelKey(undefined)).toBe('executions.evidence.unknown');
  });
});

describe('status tones', () => {
  it('paints unknown as undetermined rather than failed or completed', () => {
    expect(runtimeTone('unknown')).toBe('warn');
    expect(runtimeTone('unknown')).not.toBe(runtimeTone('failed'));
    expect(runtimeTone('unknown')).not.toBe(runtimeTone('completed'));
  });

  it('separates delivery from runtime', () => {
    expect(deliveryTone('accepted')).toBe('muted');
    expect(deliveryTone('delivered')).toBe('ok');
    expect(deliveryTone('unknown')).toBe('warn');
    expect(deliveryTone('failed')).toBe('danger');
    expect(deliveryTone('nonsense')).toBe('muted');
  });
});

describe('truncateMiddle', () => {
  it('keeps both ends of an identifier', () => {
    expect(truncateMiddle('short', 20)).toBe('short');
    const long = 'exec-0123456789abcdefghijklmnop';
    const out = truncateMiddle(long, 20);
    expect(out).toHaveLength(20);
    expect(out.startsWith('exec-')).toBe(true);
    expect(out.endsWith('mnop')).toBe(true);
  });
});

describe('label maps', () => {
  it('covers exactly the note codes the server emits', () => {
    // Mirrors the note* constants in internal/admin/executions.go. A code added
    // there without a label here would render raw, so both sets are pinned.
    expect(Object.keys(NOTE_LABEL_KEYS).sort()).toEqual([
      'effects_not_configured',
      'effects_truncated',
      'effects_unavailable',
      'events_not_configured',
      'events_truncated',
      'events_unavailable',
      'no_delivery_planned',
    ]);
    for (const code of Object.keys(NOTE_LABEL_KEYS)) {
      expect(noteLabelKey(code)).not.toBeNull();
    }
  });

  it('covers exactly the actions the server can offer', () => {
    const kinds = [
      'effect_abandon',
      'effect_mark_delivered',
      'effect_requeue',
      'fence_abandon',
      'fence_resolve',
      'queue_cancel',
    ];
    expect(Object.keys(ACTION_LABEL_KEYS).sort()).toEqual(kinds);
    expect(Object.keys(ACTION_CONFIRM_KEYS).sort()).toEqual(kinds);
    for (const kind of kinds) {
      expect(actionLabelKey(kind)).not.toBeNull();
      expect(actionConfirmKey(kind)).not.toBeNull();
    }
  });

  it('resolves every status and phase the projection emits', () => {
    for (const status of Object.keys(STATUS_LABEL_KEYS)) {
      expect(statusLabelKey(status)).not.toBeNull();
    }
    for (const phase of Object.keys(PHASE_LABEL_KEYS)) {
      expect(phaseLabelKey(phase)).not.toBeNull();
    }
  });

  it('returns null for anything unknown, so the caller can show it verbatim', () => {
    expect(noteLabelKey('a_note_from_a_newer_server')).toBeNull();
    expect(phaseLabelKey('a_new_phase')).toBeNull();
    expect(actionLabelKey('force_success')).toBeNull();
    expect(actionConfirmKey('force_success')).toBeNull();
    expect(statusLabelKey('')).toBeNull();
  });
});

describe('actionRequest', () => {
  const submission = { reason: '  operator note  ', evidenceRef: ' ref-1 ' };

  it('maps fence actions onto the fence endpoint with the version token', () => {
    const action: AdminAvailableAction = {
      kind: 'fence_resolve',
      target: 'exec-1',
      requires_version: 7,
      description: 'Clear the fence; the run stays unknown.',
    };
    const request = actionRequest(action, submission);
    expect(request?.path).toBe('/admin/executions/exec-1/fence-action');
    expect(request?.body).toEqual({
      decision: 'resolve',
      expected_fence_version: 7,
      reason: 'operator note',
      evidence_ref: 'ref-1',
    });
  });

  it('maps queue cancel onto the queue endpoint without a version token', () => {
    const request = actionRequest(
      { kind: 'queue_cancel', target: 'exec-2', description: 'Withdraw this input.' },
      { reason: 'superseded' },
    );
    expect(request?.path).toBe('/admin/executions/exec-2/queue-cancel');
    expect(request?.body).toEqual({ reason: 'superseded', evidence_ref: '' });
  });

  it('maps every effect decision onto the effect endpoint, conditional on unknown', () => {
    for (const kind of ['effect_abandon', 'effect_mark_delivered', 'effect_requeue'] as const) {
      const request = actionRequest({ kind, target: 'eff-1', requires_version: 2, description: '' }, submission);
      expect(request?.path).toBe('/admin/effects/eff-1/action');
      expect(request?.body).toEqual({
        decision: kind.replace('effect_', ''),
        expected_status: 'unknown',
        reason: 'operator note',
        evidence_ref: 'ref-1',
      });
      // requires_version is an attempt counter on the read model, not a
      // conditional token this endpoint accepts; it must not be sent.
      expect(request?.body).not.toHaveProperty('requires_version');
    }
  });

  it('percent-encodes identifiers so a crafted id cannot escape the path', () => {
    const request = actionRequest(
      { kind: 'queue_cancel', target: '../effects', description: '' },
      { reason: 'x' },
    );
    expect(request?.path).toBe('/admin/executions/..%2Feffects/queue-cancel');
  });

  it('refuses an action it does not recognise instead of approximating it', () => {
    expect(actionRequest({ kind: 'force_success', target: 'exec-1', description: '' }, submission)).toBeNull();
    expect(actionRequest({ kind: '', target: 'exec-1', description: '' }, submission)).toBeNull();
  });
});
