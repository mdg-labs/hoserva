import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

const localesDir = path.resolve(import.meta.dirname, "locales");

// JSON.parse keeps the last of two equal keys without saying so, so the
// source text is scanned instead of the parsed value.
function duplicateKeys(source: string): string[] {
  const duplicates: string[] = [];
  const scopes: Array<{ keys: Set<string>; path: string } | null> = [];
  const pathParts: string[] = [];
  let pendingKey: string | null = null;
  let i = 0;
  while (i < source.length) {
    const ch = source[i];
    if (ch === '"') {
      let j = i + 1;
      while (source[j] !== '"') {
        j += source[j] === "\\" ? 2 : 1;
      }
      const text = JSON.parse(source.slice(i, j + 1)) as string;
      const scope = scopes[scopes.length - 1];
      let k = j + 1;
      while (/\s/.test(source[k] ?? "")) {
        k++;
      }
      if (scope && source[k] === ":") {
        const full = [...pathParts, text].join(".");
        if (scope.keys.has(text)) {
          duplicates.push(full);
        }
        scope.keys.add(text);
        pendingKey = text;
      }
      i = j + 1;
      continue;
    }
    if (ch === "{") {
      if (pendingKey !== null) {
        pathParts.push(pendingKey);
      }
      scopes.push({ keys: new Set(), path: pendingKey ?? "" });
      pendingKey = null;
    } else if (ch === "[") {
      scopes.push(null);
      pendingKey = null;
    } else if (ch === "}" || ch === "]") {
      const closed = scopes.pop();
      if (ch === "}" && closed && closed.path !== "") {
        pathParts.pop();
      }
      pendingKey = null;
    } else if (ch === ",") {
      pendingKey = null;
    }
    i++;
  }
  return duplicates;
}

describe("duplicateKeys", () => {
  it("reports a key repeated in one object, by its full path", () => {
    expect(duplicateKeys('{"a":{"x":"1","y":"2","x":"3"}}')).toEqual(["a.x"]);
  });

  it("allows the same key in different objects", () => {
    expect(duplicateKeys('{"a":{"x":"1"},"b":{"x":"2"},"c":[{"x":"3"},{"x":"4"}]}')).toEqual([]);
  });

  it("does not read a colon inside a string value as a key", () => {
    expect(duplicateKeys('{"a":"x","b":"x: y","x":"z"}')).toEqual([]);
  });
});

describe("locale catalogs", () => {
  const files = readdirSync(localesDir).filter((name) => name.endsWith(".json"));

  it("finds at least the English catalog", () => {
    expect(files).toContain("en.json");
  });

  for (const file of files) {
    it(`${file} defines no key twice in one object`, () => {
      expect(duplicateKeys(readFileSync(path.join(localesDir, file), "utf8"))).toEqual([]);
    });
  }
});
