/**
 * AEP v1 cross-SDK conformance test (issue #869).
 *
 * Loads the golden corpus fixtures from pkg/aep/schema/corpus/ and validates
 * that every envelope parses correctly through the TypeScript SDK.
 * Unknown additive kinds must not throw (forward compatibility).
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { AEP_VERSION, EventKind } from '../src/constants.js';
import type { InputAckData } from '../src/types.js';

const __dirname = fileURLToPath(new URL('.', import.meta.url));

// Resolve corpus dir: tests/ -> typescript-client/ -> examples/ -> repo root
const CORPUS_DIR = resolve(__dirname, '..', '..', '..', 'pkg', 'aep', 'schema', 'corpus');

function loadCorpus(): Array<{ name: string; envelope: Record<string, unknown> }> {
  const fixtures: Array<{ name: string; envelope: Record<string, unknown> }> = [];
  const files = readdirSync(CORPUS_DIR).filter((f) => f.endsWith('.json')).sort();
  for (const name of files) {
    const raw = readFileSync(join(CORPUS_DIR, name), 'utf-8');
    fixtures.push({ name, envelope: JSON.parse(raw) });
  }
  return fixtures;
}

const CORPUS = loadCorpus();

describe('AEP corpus conformance', () => {
  it('corpus directory exists and is non-empty', () => {
    expect(CORPUS.length).toBeGreaterThan(0);
  });

  it.each(CORPUS.map((f) => [f.name, f.envelope]))(
    '%s has valid AEP envelope structure',
    (_name, envelope) => {
      const env = envelope as Record<string, unknown>;
      expect(env['version']).toBe(AEP_VERSION);
      expect(typeof env['id']).toBe('string');
      expect(env['event']).toBeDefined();
      const event = env['event'] as Record<string, unknown>;
      expect(typeof event['type']).toBe('string');
    },
  );

  it('covers all stable kinds (>= 32 non-edge-case fixtures)', () => {
    const stableKinds = new Set<string>();
    for (const { name, envelope } of CORPUS) {
      if (name.startsWith('9')) continue;
      const event = envelope['event'] as Record<string, unknown>;
      stableKinds.add(event['type'] as string);
    }
    expect(stableKinds.size).toBeGreaterThanOrEqual(32);
  });

  it('EventKind constants cover all non-edge-case corpus kinds', () => {
    const registeredValues = new Set(Object.values(EventKind));
    const missing: string[] = [];
    for (const { name, envelope } of CORPUS) {
      if (name.startsWith('9')) continue; // skip edge-case fixtures
      const event = envelope['event'] as Record<string, unknown>;
      const kind = event['type'] as string;
      if (!registeredValues.has(kind)) {
        missing.push(kind);
      }
    }
    expect(missing, `EventKind missing values: ${missing.join(', ')}`).toEqual([]);
  });

  it('unknown kind is safely ignorable (forward compatibility)', () => {
    const raw = readFileSync(join(CORPUS_DIR, '90-compatibility-unknown-kind.json'), 'utf-8');
    const env = JSON.parse(raw) as Record<string, unknown>;
    const event = env['event'] as Record<string, unknown>;
    expect(event['type']).toBe('custom.future_event');
    // Parsing succeeds — forward-compatible.
  });

  it('input.ack carries mode and durability so a buffered accept is not read as delivered', () => {
    const raw = readFileSync(
      join(CORPUS_DIR, '93-compatibility-input-ack-buffered.json'),
      'utf-8',
    );
    const env = JSON.parse(raw) as { event: { data: InputAckData } };
    const ack = env.event.data;
    expect(ack.status).toBe('accepted');
    expect(ack.input_mode).toBe('buffered');
    expect(ack.durability).toBe('volatile');
    expect(ack.parent_execution_id).toBe('exec_parent_1');
    // The volatile acceptance is explicitly NOT a delivery.
    expect(ack.status).not.toBe('delivered');
  });

  it('input.ack from a pre-receipt server decodes with the fields absent', () => {
    const raw = readFileSync(
      join(CORPUS_DIR, '94-compatibility-input-ack-legacy.json'),
      'utf-8',
    );
    const env = JSON.parse(raw) as { event: { data: InputAckData } };
    const ack = env.event.data;
    expect(ack.status).toBe('delivered');
    expect(ack.input_mode).toBeUndefined();
    expect(ack.durability).toBeUndefined();
    expect(ack.parent_execution_id).toBeUndefined();
  });
});
