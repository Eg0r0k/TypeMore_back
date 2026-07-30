"use strict";
var TypeMoreCore = (() => {
  var __defProp = Object.defineProperty;
  var __defProps = Object.defineProperties;
  var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
  var __getOwnPropDescs = Object.getOwnPropertyDescriptors;
  var __getOwnPropNames = Object.getOwnPropertyNames;
  var __getOwnPropSymbols = Object.getOwnPropertySymbols;
  var __hasOwnProp = Object.prototype.hasOwnProperty;
  var __propIsEnum = Object.prototype.propertyIsEnumerable;
  var __knownSymbol = (name, symbol) => (symbol = Symbol[name]) ? symbol : Symbol.for("Symbol." + name);
  var __typeError = (msg) => {
    throw TypeError(msg);
  };
  var __defNormalProp = (obj, key, value) => key in obj ? __defProp(obj, key, { enumerable: true, configurable: true, writable: true, value }) : obj[key] = value;
  var __spreadValues = (a, b) => {
    for (var prop in b || (b = {}))
      if (__hasOwnProp.call(b, prop))
        __defNormalProp(a, prop, b[prop]);
    if (__getOwnPropSymbols)
      for (var prop of __getOwnPropSymbols(b)) {
        if (__propIsEnum.call(b, prop))
          __defNormalProp(a, prop, b[prop]);
      }
    return a;
  };
  var __spreadProps = (a, b) => __defProps(a, __getOwnPropDescs(b));
  var __export = (target, all) => {
    for (var name in all)
      __defProp(target, name, { get: all[name], enumerable: true });
  };
  var __copyProps = (to, from, except, desc) => {
    if (from && typeof from === "object" || typeof from === "function") {
      for (let key of __getOwnPropNames(from))
        if (!__hasOwnProp.call(to, key) && key !== except)
          __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
    }
    return to;
  };
  var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);
  var __await = function(promise, isYieldStar) {
    this[0] = promise;
    this[1] = isYieldStar;
  };
  var __yieldStar = (value) => {
    var obj = value[__knownSymbol("asyncIterator")], isAwait = false, method, it = {};
    if (obj == null) {
      obj = value[__knownSymbol("iterator")]();
      method = (k) => it[k] = (x) => obj[k](x);
    } else {
      obj = obj.call(value);
      method = (k) => it[k] = (v) => {
        if (isAwait) {
          isAwait = false;
          if (k === "throw") throw v;
          return v;
        }
        isAwait = true;
        return {
          done: false,
          value: new __await(new Promise((resolve) => {
            var x = obj[k](v);
            if (!(x instanceof Object)) __typeError("Object expected");
            resolve(x);
          }), 1)
        };
      };
    }
    return it[__knownSymbol("iterator")] = () => it, method("next"), "throw" in obj ? method("throw") : it.throw = (x) => {
      throw x;
    }, "return" in obj && method("return"), it;
  };

  // src/index.ts
  var index_exports = {};
  __export(index_exports, {
    CODE_MAX_EXTRA_CHARS: () => CODE_MAX_EXTRA_CHARS,
    CORE_PACKAGE_VERSION: () => CORE_PACKAGE_VERSION,
    DEFAULT_MAX_EXTRA_CHARS: () => DEFAULT_MAX_EXTRA_CHARS,
    DEFAULT_THRESHOLDS: () => DEFAULT_THRESHOLDS,
    EQUIVALENCE_GROUPS: () => EQUIVALENCE_GROUPS,
    EVENT_LOG_VERSION: () => EVENT_LOG_VERSION,
    EVENT_LOG_VERSION_TELEMETRY: () => EVENT_LOG_VERSION_TELEMETRY,
    GameCore: () => GameCore,
    KEY_INTERVAL_CAP_MS: () => KEY_INTERVAL_CAP_MS,
    MINSPEED_GRACE_MS: () => MINSPEED_GRACE_MS,
    MINSPEED_MULTIPLIERS: () => MINSPEED_MULTIPLIERS,
    MOD_MULTIPLIERS: () => MOD_MULTIPLIERS,
    MOD_MULTIPLIER_CAP: () => MOD_MULTIPLIER_CAP,
    SCORE_VERSION: () => SCORE_VERSION,
    SCORE_VERSION_2: () => SCORE_VERSION_2,
    TICK_INTERVAL_MS: () => TICK_INTERVAL_MS,
    activeModsV1: () => activeModsV1,
    afkBetween: () => afkBetween,
    afkOf: () => afkOf,
    afkStatsOf: () => afkStatsOf,
    analyzeLog: () => analyzeLog,
    areGraphemesEquivalent: () => areGraphemesEquivalent,
    asMs: () => asMs,
    asSeq: () => asSeq,
    bufferOf: () => bufferOf,
    charObservationsOf: () => charObservationsOf,
    comboMultiplier: () => comboMultiplier,
    commitEvent: () => commitEvent,
    computeMetrics: () => computeMetrics,
    consistencyOf: () => consistencyOf,
    deleteEvent: () => deleteEvent,
    dictVersion: () => dictVersion,
    emitsRawTokens: () => emitsRawTokens,
    endsLine: () => endsLine,
    errorWords: () => errorWords,
    errorWordsOf: () => errorWordsOf,
    finalizeScore: () => finalizeScore,
    finalizeScoreV2: () => finalizeScoreV2,
    fnv1a: () => fnv1a,
    foldLog: () => foldLog,
    generateWords: () => generateWords,
    gradeOf: () => gradeOf,
    initialScoreState: () => initialScoreState,
    initialState: () => initialState,
    initialStateOf: () => initialStateOf,
    insertEvent: () => insertEvent,
    isSpaceGrapheme: () => isSpaceGrapheme,
    isTelemetryEvent: () => isTelemetryEvent,
    keyDownEvent: () => keyDownEvent,
    keyUpEvent: () => keyUpEvent,
    kogasa: () => kogasa,
    makeNormalizer: () => makeNormalizer,
    makeSeedContext: () => makeSeedContext,
    metricsFrom: () => metricsFrom,
    metricsOf: () => metricsOf,
    minSpeedFailInstant: () => minSpeedFailInstant,
    modMultiplierV1: () => modMultiplierV1,
    mulberry32: () => mulberry32,
    netCharsOf: () => netCharsOf,
    nextTickDelay: () => nextTickDelay,
    normalizeGrapheme: () => normalizeGrapheme,
    parseEventBatch: () => parseEventBatch,
    parseGameEvent: () => parseGameEvent,
    progressOf: () => progressOf,
    quoteOf: () => quoteOf,
    reduce: () => reduce,
    replaceEvent: () => replaceEvent,
    reverseWord: () => reverseWord,
    scoreOfLog: () => scoreOfLog,
    scoreStep: () => scoreStep,
    scoreV2OfLog: () => scoreV2OfLog,
    separatorsOf: () => separatorsOf,
    settle: () => settle,
    sortEvents: () => sortEvents,
    targetCharsOf: () => targetCharsOf,
    timelineFrom: () => timelineFrom,
    timelineOf: () => timelineOf,
    totalTargetCharsOf: () => totalTargetCharsOf,
    validateLog: () => validateLog,
    wordHistory: () => wordHistory,
    wordHistoryFrom: () => wordHistoryFrom,
    wordHistoryOf: () => wordHistoryOf,
    wpmOverTime: () => wpmOverTime
  });

  // src/events.ts
  var asSeq = (n) => n;
  var asMs = (n) => n;
  var isTelemetryEvent = (event) => event.kind === "down" || event.kind === "up";
  var EVENT_LOG_VERSION = 1;
  var EVENT_LOG_VERSION_TELEMETRY = 2;
  var insertEvent = (seq, t, text) => ({
    kind: "insert",
    seq: asSeq(seq),
    t: asMs(t),
    text
  });
  var deleteEvent = (seq, t, unit = "char") => ({
    kind: "delete",
    seq: asSeq(seq),
    t: asMs(t),
    unit
  });
  var commitEvent = (seq, t) => ({
    kind: "commit",
    seq: asSeq(seq),
    t: asMs(t)
  });
  var replaceEvent = (seq, t, from, to, text, source) => ({
    kind: "replace",
    seq: asSeq(seq),
    t: asMs(t),
    from,
    to,
    text,
    source
  });
  var keyDownEvent = (seq, t, code) => ({
    kind: "down",
    seq: asSeq(seq),
    t: asMs(t),
    code
  });
  var keyUpEvent = (seq, t, code) => ({
    kind: "up",
    seq: asSeq(seq),
    t: asMs(t),
    code
  });
  function sortEvents(events) {
    for (let i = 1; i < events.length; i++) {
      if (events[i].seq < events[i - 1].seq) return [...events].sort((a, b) => a.seq - b.seq);
    }
    return events;
  }

  // src/version.ts
  var CORE_PACKAGE_VERSION = true ? "2.0.0" : "0.0.0-dev";

  // src/normalize.ts
  var SPACE_CHARS = [
    " ",
    // regular space
    "\xA0",
    // no-break space
    "\u1680",
    // ogham space mark
    "\u2002",
    // en space
    "\u2003",
    // em space
    "\u2004",
    // three-per-em space
    "\u2007",
    // figure space
    "\u2008",
    // punctuation space
    "\u2009",
    // thin space
    "\u200A",
    // hair space
    "\u200B",
    // zero width space
    "\u202F",
    // narrow no-break space
    "\u3000",
    // ideographic space (CJK IME)
    "\uFEFF"
    // zero width no-break space
  ];
  var EQUIVALENCE_GROUPS = [
    { id: "apostrophes", chars: ["'", "\u2019", "\u2018", "\u02BC", "\u05F3", "\u02BB", "\u1FBD"] },
    { id: "double-quotes", chars: ['"', "\u201D", "\u201C", "\u201E"] },
    { id: "dashes", chars: ["-", "\u2013", "\u2014", "\u2010", "\u2011"] },
    { id: "commas", chars: [",", "\u201A"] },
    { id: "spaces", chars: SPACE_CHARS, canonical: " " },
    // Latin 'e' included like monkeytype: a latin layout mid-cyrillic word is a
    // layout slip, not a different letter.
    { id: "ru-yo", chars: ["\u0451", "\u0435", "e"], languages: ["russian"] }
  ];
  var appliesTo = (group, language) => {
    if (group.languages === void 0) return true;
    if (language === void 0) return false;
    return group.languages.some((name) => language === name || language.startsWith(`${name}_`));
  };
  function makeNormalizer(groups) {
    const byChar = /* @__PURE__ */ new Map();
    for (const group of groups) {
      for (const char of group.chars) {
        const list = byChar.get(char);
        if (list) list.push(group);
        else byChar.set(char, [group]);
      }
    }
    const areEquivalent = (a, b, language) => {
      if (a === b) return true;
      const candidates = byChar.get(a);
      if (candidates === void 0) return false;
      return candidates.some((group) => group.chars.includes(b) && appliesTo(group, language));
    };
    const normalize = (typed, expected, language) => {
      var _a, _b;
      if (typed === expected) return typed;
      if (expected !== void 0 && areEquivalent(typed, expected, language)) return expected;
      const fallback = (_a = byChar.get(typed)) == null ? void 0 : _a.find((group) => group.canonical !== void 0 && appliesTo(group, language));
      return (_b = fallback == null ? void 0 : fallback.canonical) != null ? _b : typed;
    };
    return { areEquivalent, normalize };
  }
  var defaultNormalizer = makeNormalizer(EQUIVALENCE_GROUPS);
  var areGraphemesEquivalent = defaultNormalizer.areEquivalent;
  var normalizeGrapheme = defaultNormalizer.normalize;
  var SPACE_SET = new Set(SPACE_CHARS);
  var isSpaceGrapheme = (grapheme) => SPACE_SET.has(grapheme);

  // ../../node_modules/.pnpm/neverthrow@8.2.0/node_modules/neverthrow/dist/index.es.js
  var defaultErrorConfig = {
    withStackTrace: false
  };
  var createNeverThrowError = (message, result, config = defaultErrorConfig) => {
    const data = result.isOk() ? { type: "Ok", value: result.value } : { type: "Err", value: result.error };
    const maybeStack = config.withStackTrace ? new Error().stack : void 0;
    return {
      data,
      message,
      stack: maybeStack
    };
  };
  function __awaiter(thisArg, _arguments, P, generator) {
    function adopt(value) {
      return value instanceof P ? value : new P(function(resolve) {
        resolve(value);
      });
    }
    return new (P || (P = Promise))(function(resolve, reject) {
      function fulfilled(value) {
        try {
          step(generator.next(value));
        } catch (e) {
          reject(e);
        }
      }
      function rejected(value) {
        try {
          step(generator["throw"](value));
        } catch (e) {
          reject(e);
        }
      }
      function step(result) {
        result.done ? resolve(result.value) : adopt(result.value).then(fulfilled, rejected);
      }
      step((generator = generator.apply(thisArg, _arguments || [])).next());
    });
  }
  function __values(o) {
    var s = typeof Symbol === "function" && Symbol.iterator, m = s && o[s], i = 0;
    if (m) return m.call(o);
    if (o && typeof o.length === "number") return {
      next: function() {
        if (o && i >= o.length) o = void 0;
        return { value: o && o[i++], done: !o };
      }
    };
    throw new TypeError(s ? "Object is not iterable." : "Symbol.iterator is not defined.");
  }
  function __await2(v) {
    return this instanceof __await2 ? (this.v = v, this) : new __await2(v);
  }
  function __asyncGenerator(thisArg, _arguments, generator) {
    if (!Symbol.asyncIterator) throw new TypeError("Symbol.asyncIterator is not defined.");
    var g = generator.apply(thisArg, _arguments || []), i, q = [];
    return i = Object.create((typeof AsyncIterator === "function" ? AsyncIterator : Object).prototype), verb("next"), verb("throw"), verb("return", awaitReturn), i[Symbol.asyncIterator] = function() {
      return this;
    }, i;
    function awaitReturn(f) {
      return function(v) {
        return Promise.resolve(v).then(f, reject);
      };
    }
    function verb(n, f) {
      if (g[n]) {
        i[n] = function(v) {
          return new Promise(function(a, b) {
            q.push([n, v, a, b]) > 1 || resume(n, v);
          });
        };
        if (f) i[n] = f(i[n]);
      }
    }
    function resume(n, v) {
      try {
        step(g[n](v));
      } catch (e) {
        settle2(q[0][3], e);
      }
    }
    function step(r) {
      r.value instanceof __await2 ? Promise.resolve(r.value.v).then(fulfill, reject) : settle2(q[0][2], r);
    }
    function fulfill(value) {
      resume("next", value);
    }
    function reject(value) {
      resume("throw", value);
    }
    function settle2(f, v) {
      if (f(v), q.shift(), q.length) resume(q[0][0], q[0][1]);
    }
  }
  function __asyncDelegator(o) {
    var i, p;
    return i = {}, verb("next"), verb("throw", function(e) {
      throw e;
    }), verb("return"), i[Symbol.iterator] = function() {
      return this;
    }, i;
    function verb(n, f) {
      i[n] = o[n] ? function(v) {
        return (p = !p) ? { value: __await2(o[n](v)), done: false } : f ? f(v) : v;
      } : f;
    }
  }
  function __asyncValues(o) {
    if (!Symbol.asyncIterator) throw new TypeError("Symbol.asyncIterator is not defined.");
    var m = o[Symbol.asyncIterator], i;
    return m ? m.call(o) : (o = typeof __values === "function" ? __values(o) : o[Symbol.iterator](), i = {}, verb("next"), verb("throw"), verb("return"), i[Symbol.asyncIterator] = function() {
      return this;
    }, i);
    function verb(n) {
      i[n] = o[n] && function(v) {
        return new Promise(function(resolve, reject) {
          v = o[n](v), settle2(resolve, reject, v.done, v.value);
        });
      };
    }
    function settle2(resolve, reject, d, v) {
      Promise.resolve(v).then(function(v2) {
        resolve({ value: v2, done: d });
      }, reject);
    }
  }
  var ResultAsync = class _ResultAsync {
    constructor(res) {
      this._promise = res;
    }
    static fromSafePromise(promise) {
      const newPromise = promise.then((value) => new Ok(value));
      return new _ResultAsync(newPromise);
    }
    static fromPromise(promise, errorFn) {
      const newPromise = promise.then((value) => new Ok(value)).catch((e) => new Err(errorFn(e)));
      return new _ResultAsync(newPromise);
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    static fromThrowable(fn, errorFn) {
      return (...args) => {
        return new _ResultAsync((() => __awaiter(this, void 0, void 0, function* () {
          try {
            return new Ok(yield fn(...args));
          } catch (error) {
            return new Err(errorFn ? errorFn(error) : error);
          }
        }))());
      };
    }
    static combine(asyncResultList) {
      return combineResultAsyncList(asyncResultList);
    }
    static combineWithAllErrors(asyncResultList) {
      return combineResultAsyncListWithAllErrors(asyncResultList);
    }
    map(f) {
      return new _ResultAsync(this._promise.then((res) => __awaiter(this, void 0, void 0, function* () {
        if (res.isErr()) {
          return new Err(res.error);
        }
        return new Ok(yield f(res.value));
      })));
    }
    andThrough(f) {
      return new _ResultAsync(this._promise.then((res) => __awaiter(this, void 0, void 0, function* () {
        if (res.isErr()) {
          return new Err(res.error);
        }
        const newRes = yield f(res.value);
        if (newRes.isErr()) {
          return new Err(newRes.error);
        }
        return new Ok(res.value);
      })));
    }
    andTee(f) {
      return new _ResultAsync(this._promise.then((res) => __awaiter(this, void 0, void 0, function* () {
        if (res.isErr()) {
          return new Err(res.error);
        }
        try {
          yield f(res.value);
        } catch (e) {
        }
        return new Ok(res.value);
      })));
    }
    orTee(f) {
      return new _ResultAsync(this._promise.then((res) => __awaiter(this, void 0, void 0, function* () {
        if (res.isOk()) {
          return new Ok(res.value);
        }
        try {
          yield f(res.error);
        } catch (e) {
        }
        return new Err(res.error);
      })));
    }
    mapErr(f) {
      return new _ResultAsync(this._promise.then((res) => __awaiter(this, void 0, void 0, function* () {
        if (res.isOk()) {
          return new Ok(res.value);
        }
        return new Err(yield f(res.error));
      })));
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    andThen(f) {
      return new _ResultAsync(this._promise.then((res) => {
        if (res.isErr()) {
          return new Err(res.error);
        }
        const newValue = f(res.value);
        return newValue instanceof _ResultAsync ? newValue._promise : newValue;
      }));
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    orElse(f) {
      return new _ResultAsync(this._promise.then((res) => __awaiter(this, void 0, void 0, function* () {
        if (res.isErr()) {
          return f(res.error);
        }
        return new Ok(res.value);
      })));
    }
    match(ok2, _err) {
      return this._promise.then((res) => res.match(ok2, _err));
    }
    unwrapOr(t) {
      return this._promise.then((res) => res.unwrapOr(t));
    }
    /**
     * @deprecated will be removed in 9.0.0.
     *
     * You can use `safeTry` without this method.
     * @example
     * ```typescript
     * safeTry(async function* () {
     *   const okValue = yield* yourResult
     * })
     * ```
     * Emulates Rust's `?` operator in `safeTry`'s body. See also `safeTry`.
     */
    safeUnwrap() {
      return __asyncGenerator(this, arguments, function* safeUnwrap_1() {
        return yield __await2(yield __await2(yield* __yieldStar(__asyncDelegator(__asyncValues(yield __await2(this._promise.then((res) => res.safeUnwrap())))))));
      });
    }
    // Makes ResultAsync implement PromiseLike<Result>
    then(successCallback, failureCallback) {
      return this._promise.then(successCallback, failureCallback);
    }
    [Symbol.asyncIterator]() {
      return __asyncGenerator(this, arguments, function* _a() {
        const result = yield __await2(this._promise);
        if (result.isErr()) {
          yield yield __await2(errAsync(result.error));
        }
        return yield __await2(result.value);
      });
    }
  };
  function errAsync(err2) {
    return new ResultAsync(Promise.resolve(new Err(err2)));
  }
  var fromPromise = ResultAsync.fromPromise;
  var fromSafePromise = ResultAsync.fromSafePromise;
  var fromAsyncThrowable = ResultAsync.fromThrowable;
  var combineResultList = (resultList) => {
    let acc = ok([]);
    for (const result of resultList) {
      if (result.isErr()) {
        acc = err(result.error);
        break;
      } else {
        acc.map((list) => list.push(result.value));
      }
    }
    return acc;
  };
  var combineResultAsyncList = (asyncResultList) => ResultAsync.fromSafePromise(Promise.all(asyncResultList)).andThen(combineResultList);
  var combineResultListWithAllErrors = (resultList) => {
    let acc = ok([]);
    for (const result of resultList) {
      if (result.isErr() && acc.isErr()) {
        acc.error.push(result.error);
      } else if (result.isErr() && acc.isOk()) {
        acc = err([result.error]);
      } else if (result.isOk() && acc.isOk()) {
        acc.value.push(result.value);
      }
    }
    return acc;
  };
  var combineResultAsyncListWithAllErrors = (asyncResultList) => ResultAsync.fromSafePromise(Promise.all(asyncResultList)).andThen(combineResultListWithAllErrors);
  var Result;
  (function(Result6) {
    function fromThrowable2(fn, errorFn) {
      return (...args) => {
        try {
          const result = fn(...args);
          return ok(result);
        } catch (e) {
          return err(errorFn ? errorFn(e) : e);
        }
      };
    }
    Result6.fromThrowable = fromThrowable2;
    function combine(resultList) {
      return combineResultList(resultList);
    }
    Result6.combine = combine;
    function combineWithAllErrors(resultList) {
      return combineResultListWithAllErrors(resultList);
    }
    Result6.combineWithAllErrors = combineWithAllErrors;
  })(Result || (Result = {}));
  function ok(value) {
    return new Ok(value);
  }
  function err(err2) {
    return new Err(err2);
  }
  var Ok = class {
    constructor(value) {
      this.value = value;
    }
    isOk() {
      return true;
    }
    isErr() {
      return !this.isOk();
    }
    map(f) {
      return ok(f(this.value));
    }
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    mapErr(_f) {
      return ok(this.value);
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    andThen(f) {
      return f(this.value);
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    andThrough(f) {
      return f(this.value).map((_value) => this.value);
    }
    andTee(f) {
      try {
        f(this.value);
      } catch (e) {
      }
      return ok(this.value);
    }
    orTee(_f) {
      return ok(this.value);
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    orElse(_f) {
      return ok(this.value);
    }
    asyncAndThen(f) {
      return f(this.value);
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    asyncAndThrough(f) {
      return f(this.value).map(() => this.value);
    }
    asyncMap(f) {
      return ResultAsync.fromSafePromise(f(this.value));
    }
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    unwrapOr(_v) {
      return this.value;
    }
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    match(ok2, _err) {
      return ok2(this.value);
    }
    safeUnwrap() {
      const value = this.value;
      return (function* () {
        return value;
      })();
    }
    _unsafeUnwrap(_) {
      return this.value;
    }
    _unsafeUnwrapErr(config) {
      throw createNeverThrowError("Called `_unsafeUnwrapErr` on an Ok", this, config);
    }
    // eslint-disable-next-line @typescript-eslint/no-this-alias, require-yield
    *[Symbol.iterator]() {
      return this.value;
    }
  };
  var Err = class {
    constructor(error) {
      this.error = error;
    }
    isOk() {
      return false;
    }
    isErr() {
      return !this.isOk();
    }
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    map(_f) {
      return err(this.error);
    }
    mapErr(f) {
      return err(f(this.error));
    }
    andThrough(_f) {
      return err(this.error);
    }
    andTee(_f) {
      return err(this.error);
    }
    orTee(f) {
      try {
        f(this.error);
      } catch (e) {
      }
      return err(this.error);
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    andThen(_f) {
      return err(this.error);
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/explicit-module-boundary-types
    orElse(f) {
      return f(this.error);
    }
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    asyncAndThen(_f) {
      return errAsync(this.error);
    }
    asyncAndThrough(_f) {
      return errAsync(this.error);
    }
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    asyncMap(_f) {
      return errAsync(this.error);
    }
    unwrapOr(v) {
      return v;
    }
    match(_ok, err2) {
      return err2(this.error);
    }
    safeUnwrap() {
      const error = this.error;
      return (function* () {
        yield err(error);
        throw new Error("Do not use this generator out of `safeTry`");
      })();
    }
    _unsafeUnwrap(config) {
      throw createNeverThrowError("Called `_unsafeUnwrap` on an Err", this, config);
    }
    _unsafeUnwrapErr(_) {
      return this.error;
    }
    *[Symbol.iterator]() {
      const self = this;
      yield self;
      return self;
    }
  };
  var fromThrowable = Result.fromThrowable;

  // src/words.ts
  function mulberry32(seed) {
    let a = seed >>> 0;
    return () => {
      a = a + 1831565813 | 0;
      let t = Math.imul(a ^ a >>> 15, 1 | a);
      t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
      return ((t ^ t >>> 14) >>> 0) / 4294967296;
    };
  }
  function fnv1a(input) {
    let h = 2166136261;
    for (let i = 0; i < input.length; i++) {
      h ^= input.charCodeAt(i);
      h = Math.imul(h, 16777619);
    }
    return h >>> 0;
  }
  function dictVersion(words) {
    return fnv1a(words.join("\0")).toString(16).padStart(8, "0");
  }
  function quoteOf(gen) {
    var _a;
    return ((_a = gen.textSource) == null ? void 0 : _a.kind) === "quote" ? gen.textSource : void 0;
  }
  function emitsRawTokens(gen) {
    var _a;
    return gen.rawTokens === true || ((_a = gen.textSource) == null ? void 0 : _a.kind) === "quote";
  }
  function makeSeedContext(dict, seed, generation) {
    const quote = quoteOf(generation);
    return {
      seed,
      dictVersion: quote ? dictVersion([quote.text]) : dictVersion(dict.words),
      generation
    };
  }
  var NUMBER_WEIGHT = 0.2;
  var PUNCTUATION_WEIGHT = 0.25;
  var SENTENCE_END = [".", "?", "!"];
  var MID_PUNCTUATION = [",", ";", ":"];
  function targetCount(gen) {
    switch (gen.mode) {
      case "words":
      case "custom":
      case "quote":
        return Math.max(1, Math.floor(gen.length));
      case "time":
        return Math.max(60, Math.ceil(gen.length * 6));
      case "free":
        return Math.max(60, Math.floor(gen.length) || 100);
    }
  }
  function randomInt(rng, max) {
    return Math.floor(rng() * max);
  }
  function decorate(word, gen, rng, capitalizeNext) {
    if (gen.numbers && rng() < NUMBER_WEIGHT) {
      const digits = 1 + randomInt(rng, 4);
      let n = "";
      for (let i = 0; i < digits; i++) n += String(randomInt(rng, 10));
      return n;
    }
    let out = word;
    if (gen.randomCase) {
      let cased = "";
      for (const ch of out) cased += rng() < 0.5 ? ch.toUpperCase() : ch.toLowerCase();
      out = cased;
    }
    if (capitalizeNext && out.length > 0) out = out[0].toUpperCase() + out.slice(1);
    if (gen.punctuation && rng() < PUNCTUATION_WEIGHT) {
      const end = SENTENCE_END[randomInt(rng, SENTENCE_END.length)];
      const mid = MID_PUNCTUATION[randomInt(rng, MID_PUNCTUATION.length)];
      out += rng() < 0.5 ? end : mid;
    }
    return out;
  }
  function reverseWord(word) {
    return [...word].reverse().join("");
  }
  function generateWords(dict, context) {
    var _a;
    const quote = quoteOf(context.generation);
    if (quote) {
      const actualHash = dictVersion([quote.text]);
      if (actualHash !== context.dictVersion) {
        return err({
          kind: "DictVersionMismatch",
          message: `quote ${quote.quoteId} text hash mismatch: context=${context.dictVersion} actual=${actualHash}`
        });
      }
      const normalized = quote.text.replace(/ *(\r\n|\r|\n) */g, "\n ");
      const words2 = normalized.split(" ").filter((word) => word.length > 0);
      if (words2.length === 0) {
        return err({ kind: "EmptyQuote", message: `quote ${quote.quoteId} has no typeable words` });
      }
      return ok({ words: words2, context, dictName: dict.name });
    }
    if (dict.words.length === 0) {
      return err({ kind: "EmptyDictionary", message: `dictionary "${dict.name}" has no words` });
    }
    const actualVersion = dictVersion(dict.words);
    if (actualVersion !== context.dictVersion) {
      return err({
        kind: "DictVersionMismatch",
        message: `dictionary version mismatch: context=${context.dictVersion} actual=${actualVersion}`
      });
    }
    const rng = mulberry32(context.seed);
    const count = targetCount(context.generation);
    const raw = emitsRawTokens(context.generation);
    const words = [];
    let prevIndex = -1;
    let capitalizeNext = !raw && context.generation.punctuation;
    for (let i = 0; i < count; i++) {
      let index = randomInt(rng, dict.words.length);
      if (index === prevIndex && dict.words.length > 1) index = (index + 1) % dict.words.length;
      prevIndex = index;
      const base = dict.words[index];
      if (raw) {
        words.push(base);
        continue;
      }
      const decorated = decorate(base, context.generation, rng, capitalizeNext);
      words.push(context.generation.reverse ? reverseWord(decorated) : decorated);
      capitalizeNext = context.generation.punctuation && SENTENCE_END.includes((_a = decorated[decorated.length - 1]) != null ? _a : "");
    }
    return ok({ words, context, dictName: dict.name });
  }

  // src/game-core.ts
  var DEFAULT_MAX_EXTRA_CHARS = 20;
  var CODE_MAX_EXTRA_CHARS = 40;
  function initialState() {
    return {
      phase: "idle",
      wordIndex: 0,
      input: [],
      startedAt: null,
      finishedAt: null,
      lastSeq: null,
      failReason: null
    };
  }
  var STATE_CORE = Symbol("gameStateBuffers");
  function coreOf(state) {
    return state[STATE_CORE];
  }
  function seek(store, version) {
    while (store.version > version) {
      const entry = store.version - 1;
      store.slots[store.jIndex[entry]] = store.jPrev[entry];
      store.len = store.jLen[entry];
      store.version = entry;
    }
    while (store.version < version) {
      const entry = store.version;
      const index = store.jIndex[entry];
      store.slots[index] = store.jNext[entry];
      if (index >= store.len) store.len = index + 1;
      store.version = entry + 1;
    }
  }
  function bufferOf(state, index) {
    var _a;
    const core = coreOf(state);
    if (core === void 0) return (_a = state.input[index]) != null ? _a : "";
    const store = core.store;
    if (store.version !== core.version) seek(store, core.version);
    return index < store.len ? store.slots[index] : "";
  }
  function materialize(core) {
    const store = core.store;
    if (store.version !== core.version) seek(store, core.version);
    return store.slots.slice(0, store.len);
  }
  function setInput(input, index, value) {
    const next = input.slice();
    next[index] = value;
    return next;
  }
  function emptyCore(words) {
    return {
      store: { words, slots: [], len: 0, version: 0, jIndex: [], jPrev: [], jNext: [], jLen: [] },
      version: 0,
      sep: 0,
      correct: 0,
      target: 0,
      cache: null
    };
  }
  function writeCore(core, index, value) {
    var _a;
    let store = core.store;
    if (core.version !== store.jIndex.length) {
      const slots = materialize(core);
      store = {
        words: store.words,
        slots,
        len: slots.length,
        version: 0,
        jIndex: [],
        jPrev: [],
        jNext: [],
        jLen: []
      };
    } else if (store.version !== core.version) {
      seek(store, core.version);
    }
    const entry = store.version;
    store.jIndex[entry] = index;
    store.jPrev[entry] = (_a = store.slots[index]) != null ? _a : "";
    store.jNext[entry] = value;
    store.jLen[entry] = store.len;
    store.slots[index] = value;
    if (index >= store.len) store.len = index + 1;
    store.version = entry + 1;
    return {
      store,
      version: entry + 1,
      sep: core.sep,
      correct: core.correct,
      target: core.target,
      cache: null
    };
  }
  function matchingChars(target, typed) {
    const shared = target.length < typed.length ? target.length : typed.length;
    let correct = 0;
    for (let k = 0; k < shared; k++) if (typed[k] === target[k]) correct++;
    return correct;
  }
  function buffersFor(ctx, state) {
    const core = coreOf(state);
    return core !== void 0 && core.store.words === ctx.words ? core : state.input;
  }
  function buffersOf(state) {
    var _a;
    return (_a = coreOf(state)) != null ? _a : state.input;
  }
  function writeBuffers(buffers, index, value) {
    return Array.isArray(buffers) ? setInput(buffers, index, value) : writeCore(buffers, index, value);
  }
  function commitBuffers(ctx, buffers, index, typed) {
    if (Array.isArray(buffers) || index >= ctx.words.length) return buffers;
    const core = buffers;
    const word = ctx.words[index];
    return {
      store: core.store,
      version: core.version,
      sep: core.sep + (endsLine(word) ? 0 : 1),
      correct: core.correct + matchingChars(word, typed),
      target: core.target + word.length,
      cache: core.cache
    };
  }
  function uncommitBuffers(ctx, buffers, index, typed) {
    if (Array.isArray(buffers) || index >= ctx.words.length) return buffers;
    const core = buffers;
    const word = ctx.words[index];
    return {
      store: core.store,
      version: core.version,
      sep: core.sep - (endsLine(word) ? 0 : 1),
      correct: core.correct - matchingChars(word, typed),
      target: core.target - word.length,
      cache: core.cache
    };
  }
  function makeState(phase, wordIndex, buffers, startedAt, finishedAt, lastSeq, failReason) {
    if (Array.isArray(buffers)) {
      return {
        phase,
        wordIndex,
        input: buffers,
        startedAt,
        finishedAt,
        lastSeq,
        failReason
      };
    }
    const core = buffers;
    const state = {
      phase,
      wordIndex,
      get input() {
        var _a;
        return (_a = core.cache) != null ? _a : core.cache = materialize(core);
      },
      startedAt,
      finishedAt,
      lastSeq,
      failReason
    };
    Object.defineProperty(state, STATE_CORE, { value: core, configurable: true });
    return state;
  }
  function initialStateOf(ctx) {
    const running = ctx.config.startPolicy === "go";
    return makeState(
      running ? "running" : "idle",
      0,
      emptyCore(ctx.words),
      running ? asMs(0) : null,
      null,
      null,
      null
    );
  }
  function withSeq(state, event) {
    return makeState(
      state.phase,
      state.wordIndex,
      buffersOf(state),
      state.startedAt,
      state.finishedAt,
      event.seq,
      state.failReason
    );
  }
  var MINSPEED_GRACE_MS = 3e3;
  function endsLine(word) {
    return word.endsWith("\n");
  }
  function separatorsOf(ctx, state) {
    var _a, _b;
    const committed = Math.min(state.wordIndex, ctx.words.length);
    const finishedByCount = state.phase === "finished" && ctx.config.mode !== "time" && ctx.config.mode !== "free";
    const core = coreOf(state);
    if (core !== void 0 && core.store.words === ctx.words) {
      const last = finishedByCount && committed > 0 && !endsLine((_a = ctx.words[committed - 1]) != null ? _a : "");
      return last ? core.sep - 1 : core.sep;
    }
    let spaces = 0;
    for (let i = 0; i < committed; i++) {
      if (finishedByCount && i === committed - 1) continue;
      if (endsLine((_b = ctx.words[i]) != null ? _b : "")) continue;
      spaces++;
    }
    return spaces;
  }
  function netCharsOf(ctx, state) {
    var _a, _b, _c, _d;
    const core = coreOf(state);
    const incremental = core !== void 0 && core.store.words === ctx.words;
    let correct = 0;
    if (incremental) {
      correct = core.correct;
    } else {
      const committed = Math.min(state.wordIndex, ctx.words.length);
      const input = state.input;
      for (let i = 0; i < committed; i++) {
        const target = (_a = ctx.words[i]) != null ? _a : "";
        const typed = (_b = input[i]) != null ? _b : "";
        const n = Math.min(target.length, typed.length);
        for (let k = 0; k < n; k++) if (typed[k] === target[k]) correct++;
      }
    }
    if (state.wordIndex < ctx.words.length) {
      const target = (_c = ctx.words[state.wordIndex]) != null ? _c : "";
      const buffer = incremental ? bufferOf(state, state.wordIndex) : (_d = state.input[state.wordIndex]) != null ? _d : "";
      const n = Math.min(target.length, buffer.length);
      for (let k = 0; k < n; k++) if (buffer[k] === target[k]) correct++;
    }
    return correct + separatorsOf(ctx, state);
  }
  function targetCharsOf(ctx, state) {
    var _a, _b, _c;
    const core = coreOf(state);
    const incremental = core !== void 0 && core.store.words === ctx.words;
    let chars = 0;
    if (incremental) {
      chars = core.target;
    } else {
      const committed = Math.min(state.wordIndex, ctx.words.length);
      for (let i = 0; i < committed; i++) chars += ((_a = ctx.words[i]) != null ? _a : "").length;
    }
    if (state.wordIndex < ctx.words.length) {
      const target = ((_b = ctx.words[state.wordIndex]) != null ? _b : "").length;
      const typed = incremental ? bufferOf(state, state.wordIndex).length : ((_c = state.input[state.wordIndex]) != null ? _c : "").length;
      chars += typed < target ? typed : target;
    }
    return chars;
  }
  function totalTargetCharsOf(ctx) {
    let chars = 0;
    for (const word of ctx.words) chars += word.length;
    return chars;
  }
  function progressOf(ctx, state) {
    const total = totalTargetCharsOf(ctx);
    if (total <= 0) return 0;
    const fraction = targetCharsOf(ctx, state) / total;
    return fraction > 1 ? 1 : fraction;
  }
  function minSpeedFailInstant(ctx, state) {
    const floor = ctx.config.minWpm;
    if (!(floor > 0) || state.startedAt === null || state.phase !== "running") return null;
    const netChars = netCharsOf(ctx, state);
    const failElapsed = Math.max(12e3 * netChars / floor, MINSPEED_GRACE_MS);
    return asMs(state.startedAt + failElapsed);
  }
  function settle(ctx, state, nowMs) {
    if (state.phase !== "running" || state.startedAt === null) return state;
    let finishAt = null;
    let reason = null;
    if (ctx.config.mode === "time") {
      finishAt = asMs(state.startedAt + ctx.config.durationMs);
    }
    if (ctx.config.minWpm > 0) {
      const failAt = minSpeedFailInstant(ctx, state);
      if (failAt !== null && (finishAt === null || failAt < finishAt)) {
        finishAt = failAt;
        reason = "minSpeed";
      }
    }
    if (finishAt !== null && nowMs >= finishAt) {
      return makeState(
        "finished",
        state.wordIndex,
        buffersOf(state),
        state.startedAt,
        finishAt,
        state.lastSeq,
        reason
      );
    }
    return state;
  }
  function hasWrongChar(target, text, at) {
    for (let k = 0; k < text.length; k++) {
      const pos = at + k;
      if (pos >= target.length || target[pos] !== text[k]) return true;
    }
    return false;
  }
  function stoppedOnError(message, event) {
    return { kind: "StoppedOnError", message, seq: event.seq };
  }
  function commitWord(ctx, event, wordIndex, buffer, buffers, startedAt) {
    var _a;
    const target = (_a = ctx.words[wordIndex]) != null ? _a : "";
    if (ctx.config.difficulty === "expert" && buffer !== target) {
      return ok(makeState("finished", wordIndex, buffers, startedAt, event.t, event.seq, "expert"));
    }
    const nextIndex = wordIndex + 1;
    const advanced = commitBuffers(ctx, buffers, wordIndex, buffer);
    const finishesByCount = ctx.config.mode !== "time" && ctx.config.mode !== "free" && nextIndex >= ctx.words.length;
    if (finishesByCount) {
      return ok(makeState("finished", nextIndex, advanced, startedAt, event.t, event.seq, null));
    }
    return ok(makeState("running", nextIndex, advanced, startedAt, null, event.seq, null));
  }
  function applyEdit(ctx, state, event, next, insertedText, insertedAt) {
    var _a, _b;
    const wordIndex = state.wordIndex;
    const target = (_a = ctx.words[wordIndex]) != null ? _a : "";
    if (next.length > target.length + ctx.config.maxExtraChars) {
      return err({
        kind: "WordLengthExceeded",
        message: `word ${wordIndex} exceeds length cap`,
        seq: event.seq
      });
    }
    const wrongInsert = hasWrongChar(target, insertedText, insertedAt);
    const master = ctx.config.difficulty === "master";
    if (wrongInsert && !master && ctx.config.stopOnError === "letter") {
      return err(stoppedOnError(`wrong character refused in word ${wordIndex}`, event));
    }
    const buffers = writeBuffers(buffersFor(ctx, state), wordIndex, next);
    const startedAt = (_b = state.startedAt) != null ? _b : event.t;
    if (master && wrongInsert) {
      return ok(makeState("finished", wordIndex, buffers, startedAt, event.t, event.seq, "master"));
    }
    const countsWords = ctx.config.mode !== "time" && ctx.config.mode !== "free";
    const isLastWord = wordIndex + 1 >= ctx.words.length;
    const autoCommits = next.length >= target.length && (ctx.config.nospace || ctx.config.quickEnd === true && countsWords && isLastWord);
    if (autoCommits) {
      return commitWord(ctx, event, wordIndex, next, buffers, startedAt);
    }
    return ok(
      makeState(
        "running",
        wordIndex,
        buffers,
        startedAt,
        state.finishedAt,
        event.seq,
        state.failReason
      )
    );
  }
  function prevWordLocked(ctx, state) {
    var _a;
    if (ctx.config.freedomMode === true) return false;
    const previous = state.wordIndex - 1;
    if (previous < 0) return false;
    return bufferOf(state, previous) === ((_a = ctx.words[previous]) != null ? _a : "");
  }
  function reduceDelete(ctx, state, event) {
    const wordIndex = state.wordIndex;
    const buffer = bufferOf(state, wordIndex);
    const crossesBoundary = buffer.length === 0 && wordIndex > 0;
    if (crossesBoundary && prevWordLocked(ctx, state)) {
      return err({
        kind: "BackspaceLocked",
        message: `backspace blocked at correct word ${wordIndex - 1}`,
        seq: event.seq
      });
    }
    const edited = (buffers, index) => makeState(
      state.phase,
      index,
      buffers,
      state.startedAt,
      state.finishedAt,
      event.seq,
      state.failReason
    );
    if (event.unit === "word") {
      if (buffer.length > 0) {
        return ok(edited(writeBuffers(buffersFor(ctx, state), wordIndex, ""), wordIndex));
      }
      if (crossesBoundary) {
        const previous = wordIndex - 1;
        const reopened = uncommitBuffers(
          ctx,
          buffersFor(ctx, state),
          previous,
          bufferOf(state, previous)
        );
        return ok(edited(writeBuffers(reopened, previous, ""), previous));
      }
      return ok(withSeq(state, event));
    }
    if (buffer.length > 0) {
      return ok(
        edited(writeBuffers(buffersFor(ctx, state), wordIndex, buffer.slice(0, -1)), wordIndex)
      );
    }
    if (crossesBoundary) {
      const previous = wordIndex - 1;
      return ok(
        edited(
          uncommitBuffers(ctx, buffersFor(ctx, state), previous, bufferOf(state, previous)),
          previous
        )
      );
    }
    return ok(withSeq(state, event));
  }
  function reduceCommit(ctx, state, event) {
    var _a;
    if (ctx.config.nospace) {
      return err({
        kind: "NospaceCommit",
        message: "nospace: word advance is derived from inserts, commits are inert",
        seq: event.seq
      });
    }
    if (state.phase !== "running") return ok(withSeq(state, event));
    const buffer = bufferOf(state, state.wordIndex);
    const stoppedOnWrongWord = ctx.config.stopOnError === "word" && buffer !== ((_a = ctx.words[state.wordIndex]) != null ? _a : "");
    if (buffer.length === 0) {
      if (stoppedOnWrongWord)
        return err(stoppedOnError(`word ${state.wordIndex} cannot be skipped`, event));
      return ok(withSeq(state, event));
    }
    if (stoppedOnWrongWord && ctx.config.difficulty !== "expert") {
      return err(stoppedOnError(`word ${state.wordIndex} is not correct yet`, event));
    }
    return commitWord(ctx, event, state.wordIndex, buffer, buffersFor(ctx, state), state.startedAt);
  }
  function reduce(ctx, state, event) {
    if (state.lastSeq !== null && event.seq <= state.lastSeq) {
      return err({
        kind: "NonMonotonicSeq",
        message: `seq ${event.seq} <= lastSeq ${state.lastSeq}`,
        seq: event.seq
      });
    }
    if (event.kind === "down" || event.kind === "up") {
      return ok(state);
    }
    if (state.phase === "finished") {
      return err({ kind: "TestFinished", message: "test already finished", seq: event.seq });
    }
    switch (event.kind) {
      case "insert": {
        const buffer = bufferOf(state, state.wordIndex);
        return applyEdit(ctx, state, event, buffer + event.text, event.text, buffer.length);
      }
      case "replace": {
        const buffer = bufferOf(state, state.wordIndex);
        if (event.from < 0 || event.to < event.from || event.to > buffer.length) {
          return err({
            kind: "InvalidRange",
            message: `replace range [${event.from},${event.to}) invalid for buffer length ${buffer.length}`,
            seq: event.seq
          });
        }
        const next = buffer.slice(0, event.from) + event.text + buffer.slice(event.to);
        return applyEdit(ctx, state, event, next, event.text, event.from);
      }
      case "delete":
        return reduceDelete(ctx, state, event);
      case "commit":
        return reduceCommit(ctx, state, event);
      default: {
        const unknown = event;
        return err({
          kind: "UnknownEventKind",
          message: `unknown event kind '${String(unknown.kind)}'`,
          seq: unknown.seq
        });
      }
    }
  }
  function foldLog(ctx, events, endMs) {
    const ordered = sortEvents(events);
    let state = initialStateOf(ctx);
    for (const event of ordered) {
      state = settle(ctx, state, event.t);
      const result = reduce(ctx, state, event);
      if (result.isErr()) return err({ error: result.error, at: event.seq });
      state = result.value;
    }
    const end = endMs != null ? endMs : ordered.length > 0 ? ordered[ordered.length - 1].t : asMs(0);
    return ok(settle(ctx, state, end));
  }
  var GameCore = class {
    constructor(init) {
      this._events = [];
      this._ctx = {
        config: __spreadValues({}, init.config),
        words: [...init.words]
      };
      this._state = initialStateOf(this._ctx);
    }
    get config() {
      return this._ctx.config;
    }
    get words() {
      return this._ctx.words;
    }
    get state() {
      return this._state;
    }
    get events() {
      return this._events;
    }
    /** Apply one event. On success, state advances and the event is logged. */
    dispatch(event) {
      const settled = settle(this._ctx, this._state, event.t);
      const result = reduce(this._ctx, settled, event);
      if (result.isOk()) {
        this._state = result.value;
        this._events.push(event);
      } else {
        this._state = settled;
      }
      return result;
    }
    /** Advance timed completion to the given instant (called by the worker tick). */
    tick(nowMs) {
      this._state = settle(this._ctx, this._state, nowMs);
      return this._state;
    }
    reset() {
      this._state = initialStateOf(this._ctx);
      this._events.length = 0;
    }
  };

  // src/keyboard.ts
  var KEY_INTERVAL_CAP_MS = 2e3;
  function charObservationsOf(ctx, events) {
    var _a;
    let state = initialStateOf(ctx);
    const byChar = /* @__PURE__ */ new Map();
    const observe = (char, wrong, intervalMs) => {
      let row = byChar.get(char);
      if (row === void 0) {
        row = { presses: 0, errors: 0, sum: 0, n: 0 };
        byChar.set(char, row);
      }
      row.presses++;
      if (wrong) row.errors++;
      if (intervalMs !== null && intervalMs >= 0 && intervalMs <= KEY_INTERVAL_CAP_MS) {
        row.sum += intervalMs;
        row.n++;
      }
    };
    const stateEvents = events.some(isTelemetryEvent) ? events.filter((e) => !isTelemetryEvent(e)) : events;
    let prevT = null;
    for (const event of sortEvents(stateEvents)) {
      state = settle(ctx, state, event.t);
      if (state.phase === "finished") break;
      if (event.kind === "insert") {
        const target = (_a = ctx.words[state.wordIndex]) != null ? _a : "";
        const startPos = bufferOf(state, state.wordIndex).length;
        for (let k = 0; k < event.text.length; k++) {
          const pos = startPos + k;
          const wrong = !(pos < target.length && target[pos] === event.text[k]);
          observe(event.text[k], wrong, k === 0 && prevT !== null ? event.t - prevT : null);
        }
      }
      const before = state.wordIndex;
      const result = reduce(ctx, state, event);
      if (result.isErr()) break;
      state = result.value;
      if (event.kind === "insert") {
        prevT = event.t;
      } else if (event.kind === "commit" && state.wordIndex > before) {
        observe(" ", false, prevT !== null ? event.t - prevT : null);
        prevT = event.t;
      } else {
        prevT = null;
      }
    }
    return [...byChar.entries()].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0).map(([char, row]) => ({
      char,
      presses: row.presses,
      errors: row.errors,
      intervalSumMs: row.sum,
      intervalCount: row.n
    }));
  }

  // src/stats.ts
  function analyzeLog(ctx, events) {
    var _a;
    let state = initialStateOf(ctx);
    let correctKeys = 0;
    let totalKeys = 0;
    let aborted = false;
    const wordFirstT = [];
    const wordLastT = [];
    const keyTimes = [];
    const keyCorrect = [];
    const keyWordIndex = [];
    const commitTimes = [];
    events = events.some(isTelemetryEvent) ? events.filter((e) => !isTelemetryEvent(e)) : events;
    for (const event of sortEvents(events)) {
      state = settle(ctx, state, event.t);
      if (state.phase === "finished") {
        aborted = true;
        break;
      }
      if (event.kind === "insert" || event.kind === "replace") {
        const wordIndex = state.wordIndex;
        const target = (_a = ctx.words[wordIndex]) != null ? _a : "";
        const startPos = event.kind === "replace" ? event.from : bufferOf(state, wordIndex).length;
        for (let k = 0; k < event.text.length; k++) {
          const pos = startPos + k;
          totalKeys++;
          const correct = pos < target.length && target[pos] === event.text[k];
          if (correct) correctKeys++;
          keyTimes.push(event.t);
          keyCorrect.push(correct);
          keyWordIndex.push(wordIndex);
        }
        if (wordFirstT[wordIndex] === void 0) wordFirstT[wordIndex] = event.t;
        wordLastT[wordIndex] = event.t;
      }
      const beforeIndex = state.wordIndex;
      const result = reduce(ctx, state, event);
      if (result.isErr()) {
        aborted = true;
        break;
      }
      state = result.value;
      for (let j = beforeIndex; j < state.wordIndex; j++) commitTimes.push(event.t);
    }
    return {
      finalState: state,
      aborted,
      correctKeys,
      totalKeys,
      wordFirstT,
      wordLastT,
      keyTimes,
      keyCorrect,
      keyWordIndex,
      commitTimes
    };
  }
  function compareWord(target, typed, includeMissed) {
    const common = Math.min(target.length, typed.length);
    let correct = 0;
    let incorrect = 0;
    for (let i = 0; i < common; i++) {
      if (typed[i] === target[i]) correct++;
      else incorrect++;
    }
    return {
      correct,
      incorrect,
      extra: Math.max(0, typed.length - target.length),
      missed: includeMissed ? Math.max(0, target.length - typed.length) : 0
    };
  }
  function getChars(ctx, state) {
    var _a, _b;
    const committed = Math.min(state.wordIndex, ctx.words.length);
    const input = state.input;
    let correct = 0;
    let incorrect = 0;
    let extra = 0;
    let missed = 0;
    for (let i = 0; i < committed; i++) {
      const word = compareWord(ctx.words[i], (_a = input[i]) != null ? _a : "", true);
      correct += word.correct;
      incorrect += word.incorrect;
      extra += word.extra;
      missed += word.missed;
    }
    if (state.wordIndex < ctx.words.length) {
      const buffer = (_b = input[state.wordIndex]) != null ? _b : "";
      if (buffer.length > 0) {
        const word = compareWord(ctx.words[state.wordIndex], buffer, false);
        correct += word.correct;
        incorrect += word.incorrect;
        extra += word.extra;
      }
    }
    return { chars: { correct, incorrect, extra, missed }, spaces: separatorsOf(ctx, state) };
  }
  function kogasa(cov) {
    return 1 - Math.tanh(cov + cov ** 3 / 3 + cov ** 5 / 5);
  }
  function consistencyOf(rawPerSecond) {
    if (rawPerSecond.length === 0) return 0;
    let sum = 0;
    for (const r of rawPerSecond) sum += r;
    const mean = sum / rawPerSecond.length;
    if (mean === 0) return 0;
    let sq = 0;
    for (const r of rawPerSecond) sq += (r - mean) ** 2;
    const value = kogasa(Math.sqrt(sq / rawPerSecond.length) / mean);
    return Number.isNaN(value) ? 0 : value;
  }
  function rawPerSecondOf(analysis, endMs) {
    var _a;
    const startedAt = analysis.finalState.startedAt;
    if (startedAt === null) return [];
    const end = (_a = analysis.finalState.finishedAt) != null ? _a : endMs;
    const seconds = Math.ceil(Math.max(0, (end - startedAt) / 1e3));
    if (seconds <= 0) return [];
    const { keyTimes } = analysis;
    const counts = new Float64Array(seconds + 1);
    for (let k = 0; k < keyTimes.length; k++) {
      const offset = keyTimes[k] - startedAt;
      if (offset < 0) continue;
      const bucket = Math.floor(offset / 1e3) + 1;
      if (bucket <= seconds) counts[bucket]++;
    }
    const out = [];
    const fullRateMin = 1e3 / 6e4;
    for (let s = 1; s < seconds; s++) out.push(counts[s] / 5 / fullRateMin);
    const bucketEnd = startedAt + seconds * 1e3;
    const checkpoint = Math.min(bucketEnd, end);
    if (checkpoint < bucketEnd) {
      const rateStart = Math.max(startedAt, checkpoint - 1e3);
      let rawInWindow = 0;
      for (let k = 0; k < keyTimes.length; k++) if (keyTimes[k] >= rateStart) rawInWindow++;
      const rateMin = (checkpoint - rateStart) / 6e4;
      out.push(rateMin > 0 ? rawInWindow / 5 / rateMin : 0);
    } else {
      out.push(counts[seconds] / 5 / fullRateMin);
    }
    return out;
  }
  function computeMetrics(ctx, events, endMs) {
    return metricsFrom(ctx, analyzeLog(ctx, events), endMs);
  }
  function metricsFrom(ctx, analysis, endMs) {
    var _a;
    const { chars, spaces } = getChars(ctx, analysis.finalState);
    const startedAt = analysis.finalState.startedAt;
    const end = (_a = analysis.finalState.finishedAt) != null ? _a : endMs;
    const durationSec = startedAt === null ? 0 : Math.max(0, (end - startedAt) / 1e3);
    const minutes = durationSec / 60;
    const netChars = chars.correct + spaces;
    const rawChars = chars.correct + chars.incorrect + chars.extra + spaces;
    return {
      wpm: minutes > 0 ? netChars / 5 / minutes : 0,
      raw: minutes > 0 ? rawChars / 5 / minutes : 0,
      accuracy: analysis.totalKeys === 0 ? 0 : analysis.correctKeys / analysis.totalKeys,
      consistency: consistencyOf(rawPerSecondOf(analysis, endMs)),
      chars,
      spaces,
      durationSec
    };
  }
  function metricsOf(core, nowMs) {
    var _a, _b, _c;
    const ctx = { config: core.config, words: core.words };
    const end = (_c = (_b = (_a = core.state.finishedAt) != null ? _a : nowMs) != null ? _b : core.state.startedAt) != null ? _c : asMs(0);
    return computeMetrics(ctx, core.events, end);
  }
  function wpmOverTime(ctx, events, endMs) {
    return timelineFrom(ctx, analyzeLog(ctx, events), endMs);
  }
  function timelineFrom(ctx, analysis, endMs) {
    var _a, _b;
    const startedAt = analysis.finalState.startedAt;
    if (startedAt === null) return [];
    const end = (_a = analysis.finalState.finishedAt) != null ? _a : endMs;
    const seconds = Math.ceil(Math.max(0, (end - startedAt) / 1e3));
    if (seconds <= 0) return [];
    const finishedByCount = analysis.finalState.phase === "finished" && ctx.config.mode !== "time" && ctx.config.mode !== "free";
    const spaceTimes = [];
    for (let i = 0; i < analysis.commitTimes.length; i++) {
      if (finishedByCount && i === analysis.commitTimes.length - 1) continue;
      if (endsLine((_b = ctx.words[i]) != null ? _b : "")) continue;
      spaceTimes.push(analysis.commitTimes[i]);
    }
    const { keyTimes, keyCorrect } = analysis;
    const rawInBucket = new Float64Array(seconds + 2);
    const errorsInBucket = new Float64Array(seconds + 2);
    const correctByCheckpoint = new Float64Array(seconds + 2);
    for (let k = 0; k < keyTimes.length; k++) {
      const offset = keyTimes[k] - startedAt;
      if (offset >= 0) {
        const bucket = Math.floor(offset / 1e3) + 1;
        if (bucket <= seconds) {
          rawInBucket[bucket]++;
          if (!keyCorrect[k]) errorsInBucket[bucket]++;
        }
      }
      if (keyCorrect[k]) {
        const from = offset <= 0 ? 0 : Math.ceil(offset / 1e3);
        if (from <= seconds) correctByCheckpoint[from]++;
      }
    }
    const spacesByCheckpoint = new Float64Array(seconds + 2);
    for (const t of spaceTimes) {
      const offset = t - startedAt;
      const from = offset <= 0 ? 0 : Math.ceil(offset / 1e3);
      if (from <= seconds) spacesByCheckpoint[from]++;
    }
    for (let s = 1; s <= seconds; s++) {
      correctByCheckpoint[s] += correctByCheckpoint[s - 1];
      spacesByCheckpoint[s] += spacesByCheckpoint[s - 1];
    }
    const points = [];
    for (let s = 1; s <= seconds; s++) {
      const bucketStart = startedAt + (s - 1) * 1e3;
      const bucketEnd = startedAt + s * 1e3;
      const checkpoint = Math.min(bucketEnd, end);
      const tail = checkpoint < bucketEnd;
      const rateStart = Math.max(startedAt, checkpoint - 1e3);
      let correctSoFar;
      let rawInWindow;
      let errors;
      let spacesSoFar;
      if (s < seconds) {
        correctSoFar = correctByCheckpoint[s];
        rawInWindow = rawInBucket[s];
        errors = errorsInBucket[s];
        spacesSoFar = spacesByCheckpoint[s];
      } else {
        correctSoFar = 0;
        rawInWindow = 0;
        errors = 0;
        spacesSoFar = 0;
        for (let k = 0; k < keyTimes.length; k++) {
          const t = keyTimes[k];
          const correct = keyCorrect[k];
          if (t <= checkpoint && correct) correctSoFar++;
          if (t >= bucketStart && t < bucketEnd && !correct) errors++;
          const inWindow = tail ? t >= rateStart : t >= bucketStart && t < bucketEnd;
          if (inWindow) rawInWindow++;
        }
        for (const t of spaceTimes) if (t <= checkpoint) spacesSoFar++;
      }
      const elapsedMin = (checkpoint - startedAt) / 6e4;
      const rateMin = (checkpoint - rateStart) / 6e4;
      points.push({
        second: s,
        wpm: elapsedMin > 0 ? (correctSoFar + spacesSoFar) / 5 / elapsedMin : 0,
        raw: rateMin > 0 ? rawInWindow / 5 / rateMin : 0,
        errors
      });
    }
    return points;
  }
  function errorWords(ctx, events) {
    var _a;
    const { finalState } = analyzeLog(ctx, events);
    const committed = Math.min(finalState.wordIndex, ctx.words.length);
    const input = finalState.input;
    const out = [];
    for (let i = 0; i < committed; i++) {
      const expected = ctx.words[i];
      const typed = (_a = input[i]) != null ? _a : "";
      if (typed !== expected) out.push({ expected, typed });
    }
    return out;
  }
  function wordHistory(ctx, events) {
    return wordHistoryFrom(ctx, analyzeLog(ctx, events));
  }
  function wordHistoryFrom(ctx, analysis) {
    var _a, _b;
    const state = analysis.finalState;
    const input = state.input;
    const committed = Math.min(state.wordIndex, ctx.words.length);
    const inFlight = state.wordIndex < ctx.words.length && ((_a = input[state.wordIndex]) != null ? _a : "") !== "" ? 1 : 0;
    const out = [];
    for (let i = 0; i < committed + inFlight; i++) {
      const typed = (_b = input[i]) != null ? _b : "";
      const first = analysis.wordFirstT[i];
      const last = analysis.wordLastT[i];
      let burst;
      if (first !== void 0 && last !== void 0 && typed.length > 0) {
        const durationMs = last - first;
        burst = durationMs > 0 ? typed.length / 5 / (durationMs / 6e4) : Infinity;
      }
      out.push({ target: ctx.words[i], typed, committed: i < committed, burst });
    }
    return out;
  }
  function wordHistoryOf(core) {
    return wordHistory({ config: core.config, words: core.words }, core.events);
  }
  function timelineOf(core, nowMs) {
    var _a, _b, _c;
    const ctx = { config: core.config, words: core.words };
    const end = (_c = (_b = (_a = core.state.finishedAt) != null ? _a : nowMs) != null ? _b : core.state.startedAt) != null ? _c : asMs(0);
    return wpmOverTime(ctx, core.events, end);
  }
  function errorWordsOf(core) {
    return errorWords({ config: core.config, words: core.words }, core.events);
  }
  var AFK_BUCKET_MS = 1e3;
  function afkOf(ctx, events, endMs) {
    var _a;
    const { finalState } = analyzeLog(ctx, events);
    return afkBetween(events, finalState.startedAt, (_a = finalState.finishedAt) != null ? _a : endMs);
  }
  function afkBetween(events, startedAt, endMs) {
    const start = startedAt;
    if (start === null) return { afkMs: 0, buckets: 0 };
    const end = endMs;
    const bucketCount = Math.floor((end - start) / AFK_BUCKET_MS);
    if (bucketCount <= 0) return { afkMs: 0, buckets: 0 };
    const active = new Uint8Array(bucketCount + 1);
    let activeCount = 0;
    for (const event of events) {
      if (isTelemetryEvent(event)) continue;
      const offset = event.t - start;
      if (offset < 0) continue;
      const bucket = offset <= 0 ? 1 : Math.ceil(offset / AFK_BUCKET_MS);
      if (bucket > bucketCount || active[bucket] === 1) continue;
      active[bucket] = 1;
      activeCount += 1;
    }
    const buckets = bucketCount - activeCount;
    return { afkMs: buckets * AFK_BUCKET_MS, buckets };
  }
  function afkStatsOf(core, nowMs) {
    var _a, _b, _c;
    const ctx = { config: core.config, words: core.words };
    return afkOf(ctx, core.events, (_c = (_b = (_a = core.state.finishedAt) != null ? _a : nowMs) != null ? _b : core.state.startedAt) != null ? _c : asMs(0));
  }

  // src/mods.ts
  var MOD_MULTIPLIER_CAP = 4;
  var MOD_MULTIPLIERS = {
    punctuation: 1.1,
    numbers: 1.08,
    nospace: 1.12,
    randomCase: 1.15,
    expert: 1.15,
    master: 1.25,
    reverse: 1.25,
    blind: 1.3,
    fading: 1.35,
    flashlight: 1.4
  };
  var MINSPEED_MULTIPLIERS = {
    60: 1.1,
    80: 1.25,
    100: 1.45
  };
  function activeModsV1(setup, declaration) {
    const { generation: g, config: c } = setup;
    const mods = [];
    const add = (id, on, multiplier) => {
      if (on) mods.push({ id, multiplier });
    };
    const transformed = !emitsRawTokens(g);
    add("punctuation", transformed && g.punctuation, MOD_MULTIPLIERS.punctuation);
    add("numbers", transformed && g.numbers, MOD_MULTIPLIERS.numbers);
    add("randomCase", transformed && g.randomCase, MOD_MULTIPLIERS.randomCase);
    add("nospace", c.nospace, MOD_MULTIPLIERS.nospace);
    add("expert", c.difficulty === "expert", MOD_MULTIPLIERS.expert);
    add("master", c.difficulty === "master", MOD_MULTIPLIERS.master);
    add("reverse", transformed && g.reverse, MOD_MULTIPLIERS.reverse);
    if (c.minWpm > 0 && MINSPEED_MULTIPLIERS[c.minWpm] !== void 0) {
      mods.push({ id: `minSpeed${c.minWpm}`, multiplier: MINSPEED_MULTIPLIERS[c.minWpm] });
    }
    add("blind", declaration.blind, MOD_MULTIPLIERS.blind);
    add("fading", declaration.fading, MOD_MULTIPLIERS.fading);
    add("flashlight", declaration.flashlight, MOD_MULTIPLIERS.flashlight);
    return mods;
  }
  function modMultiplierV1(setup, declaration) {
    const product = activeModsV1(setup, declaration).reduce((acc, mod) => acc * mod.multiplier, 1);
    return Math.min(product, MOD_MULTIPLIER_CAP);
  }

  // src/score.ts
  var SCORE_VERSION = 1;
  var SCORE_VERSION_2 = 2;
  var POINTS_PER_KEYSTROKE = 10;
  var COMBO_TIER = 25;
  var COMBO_STEP = 0.25;
  var MAX_MULTIPLIER = 2.5;
  var REFERENCE_WPM = 80;
  var CHARS_PER_WORD = 5;
  function comboMultiplier(streak) {
    const mult = 1 + COMBO_STEP * Math.floor(streak / COMBO_TIER);
    return mult > MAX_MULTIPLIER ? MAX_MULTIPLIER : mult;
  }
  function gradeOf(acc) {
    if (acc >= 1) return "SS";
    if (acc >= 0.98) return "S";
    if (acc >= 0.95) return "A";
    if (acc >= 0.9) return "B";
    return "C";
  }
  function initialScoreState() {
    return {
      base: 0,
      streak: 0,
      comboPeak: 0,
      wordIndex: 0,
      finished: false,
      bufLen: [],
      reached: []
    };
  }
  function isCountMode(mode) {
    return mode !== "time" && mode !== "free";
  }
  function advanceWord(state, ctx) {
    state.wordIndex += 1;
    if (isCountMode(ctx.config.mode) && state.wordIndex >= ctx.words.length) state.finished = true;
  }
  function applyInsert(state, text, ctx) {
    var _a, _b, _c;
    for (const char of text) {
      const wi = state.wordIndex;
      const target = (_a = ctx.words[wi]) != null ? _a : "";
      const pos = (_b = state.bufLen[wi]) != null ? _b : 0;
      const reached = (_c = state.reached[wi]) != null ? _c : 0;
      const correct = pos < target.length && target[pos] === char;
      const firstAttempt = pos >= reached;
      if (correct && firstAttempt) {
        state.streak += 1;
        if (state.streak > state.comboPeak) state.comboPeak = state.streak;
        state.base += POINTS_PER_KEYSTROKE * comboMultiplier(state.streak);
      } else if (!correct) {
        state.streak = 0;
      }
      const nextLen = pos + 1;
      state.bufLen[wi] = nextLen;
      if (nextLen > reached) state.reached[wi] = nextLen;
      if (ctx.config.nospace && nextLen >= target.length) {
        advanceWord(state, ctx);
        if (state.finished) return;
      }
    }
  }
  function applyReplace(state, from, to, text, ctx) {
    var _a, _b, _c;
    const wi = state.wordIndex;
    const bufLen = (_a = state.bufLen[wi]) != null ? _a : 0;
    const nextLen = from + text.length + (bufLen - to);
    state.bufLen[wi] = nextLen;
    if (nextLen > ((_b = state.reached[wi]) != null ? _b : 0)) state.reached[wi] = nextLen;
    const target = (_c = ctx.words[wi]) != null ? _c : "";
    if (ctx.config.nospace && nextLen >= target.length) advanceWord(state, ctx);
  }
  function applyDelete(state, unit) {
    var _a;
    const wi = state.wordIndex;
    const bufLen = (_a = state.bufLen[wi]) != null ? _a : 0;
    if (unit === "word") {
      if (bufLen > 0) state.bufLen[wi] = 0;
      else if (wi > 0) {
        state.wordIndex = wi - 1;
        state.bufLen[wi - 1] = 0;
      }
      return;
    }
    if (bufLen > 0) state.bufLen[wi] = bufLen - 1;
    else if (wi > 0) state.wordIndex = wi - 1;
  }
  function applyCommit(state, ctx) {
    var _a, _b;
    if (ctx.config.nospace) return;
    const wi = state.wordIndex;
    const bufLen = (_a = state.bufLen[wi]) != null ? _a : 0;
    if (bufLen === 0) return;
    const target = (_b = ctx.words[wi]) != null ? _b : "";
    if (bufLen < target.length) state.streak = 0;
    advanceWord(state, ctx);
  }
  function scoreStep(state, event, ctx) {
    if (state.finished) return state;
    switch (event.kind) {
      case "insert":
        applyInsert(state, event.text, ctx);
        break;
      case "replace":
        applyReplace(state, event.from, event.to, event.text, ctx);
        break;
      case "delete":
        applyDelete(state, event.unit);
        break;
      case "commit":
        applyCommit(state, ctx);
        break;
      default:
        break;
    }
    return state;
  }
  function finalizeScore(base, comboPeak, metrics, mode) {
    const accMultiplier = metrics.accuracy * metrics.accuracy;
    let timeBonus = null;
    if (isCountMode(mode)) {
      const netChars = metrics.chars.correct + metrics.spaces;
      const referenceMinutes = netChars / CHARS_PER_WORD / REFERENCE_WPM;
      const actualMinutes = metrics.durationSec / 60;
      timeBonus = actualMinutes > 0 ? referenceMinutes / actualMinutes : 1;
    }
    const total = Math.round(base * accMultiplier * (timeBonus != null ? timeBonus : 1));
    return { version: SCORE_VERSION, total, base, comboPeak, accMultiplier, timeBonus };
  }
  function scoreOfLog(log, setup) {
    const ctx = { config: setup.config, words: setup.words };
    const ordered = sortEvents(log).filter((e) => !isTelemetryEvent(e));
    const state = initialScoreState();
    for (const event of ordered) scoreStep(state, event, ctx);
    const endMs = ordered.length > 0 ? ordered[ordered.length - 1].t : asMs(0);
    const metrics = computeMetrics(ctx, ordered, endMs);
    return finalizeScore(state.base, state.comboPeak, metrics, ctx.config.mode);
  }
  function finalizeScoreV2(base, comboPeak, metrics, mode, modMultiplier) {
    var _a;
    const v1 = finalizeScore(base, comboPeak, metrics, mode);
    const total = Math.round(base * v1.accMultiplier * ((_a = v1.timeBonus) != null ? _a : 1) * modMultiplier);
    return {
      version: SCORE_VERSION_2,
      total,
      base,
      comboPeak,
      accMultiplier: v1.accMultiplier,
      timeBonus: v1.timeBonus,
      modMultiplier
    };
  }
  function scoreV2OfLog(log, setup, declaration) {
    const ctx = { config: setup.config, words: setup.words };
    const ordered = sortEvents(log).filter((e) => !isTelemetryEvent(e));
    const lastT = ordered.length > 0 ? ordered[ordered.length - 1].t : asMs(0);
    const analysis = analyzeLog(ctx, ordered);
    let end = lastT;
    if (!analysis.aborted) {
      let finalState = settle(ctx, analysis.finalState, lastT);
      if (ctx.config.minWpm > 0 && finalState.phase === "running") {
        const failAt = minSpeedFailInstant(ctx, finalState);
        if (failAt !== null) finalState = settle(ctx, finalState, failAt);
      }
      if (finalState.finishedAt !== null) end = finalState.finishedAt;
    }
    const state = initialScoreState();
    for (const event of ordered) scoreStep(state, event, ctx);
    const metrics = metricsFrom(ctx, analysis, end);
    const modMultiplier = modMultiplierV1(
      { generation: setup.generation, config: ctx.config },
      declaration
    );
    return finalizeScoreV2(state.base, state.comboPeak, metrics, ctx.config.mode, modMultiplier);
  }

  // src/timer.ts
  var TICK_INTERVAL_MS = 1e3;
  function nextTickDelay(elapsedMs, tickIndex, durationMs) {
    const targetElapsed = Math.min(tickIndex * TICK_INTERVAL_MS, durationMs);
    return Math.max(0, targetElapsed - elapsedMs);
  }

  // src/validate.ts
  var DEFAULT_THRESHOLDS = {
    minKeyIntervalMs: 15,
    uniformToleranceMs: 2,
    uniformFlagRatio: 0.9,
    maxBurstWpm: 250,
    afkFlagShare: 0.5,
    trailingAfkMs: 1e4
  };
  var ZERO_METRICS = {
    wpm: 0,
    raw: 0,
    accuracy: 0,
    consistency: 0,
    chars: { correct: 0, incorrect: 0, extra: 0, missed: 0 },
    spaces: 0,
    durationSec: 0
  };
  function graphemeCount(text) {
    return [...text].length;
  }
  function validateLog(input) {
    var _a, _b, _c, _d, _e, _f, _g, _h, _i;
    const thresholds = __spreadValues(__spreadValues({}, DEFAULT_THRESHOLDS), input.thresholds);
    const { config, generation } = input.configSnapshot;
    const seedContext = makeSeedContext(input.dictionary, input.seed, generation);
    if (input.dictVersion !== seedContext.dictVersion) {
      return err({
        kind: "DictVersionMismatch",
        message: `claimed dictVersion ${input.dictVersion} != dictionary ${seedContext.dictVersion}`
      });
    }
    const generated = generateWords(input.dictionary, seedContext);
    if (generated.isErr()) return err({ kind: "GenerationFailed", message: generated.error.message });
    const ctx = { config, words: generated.value.words };
    const events = sortEvents(input.log.events);
    const stateEvents = events.filter((e) => !isTelemetryEvent(e));
    const telemetry = events.filter(isTelemetryEvent);
    const flags = [];
    const invalid = (reason) => ok({ verdict: "invalid", reason, flags, metrics: ZERO_METRICS });
    if (input.log.version !== EVENT_LOG_VERSION && input.log.version !== EVENT_LOG_VERSION_TELEMETRY) {
      return invalid(`log version ${input.log.version} != ${EVENT_LOG_VERSION}`);
    }
    if (input.log.version === EVENT_LOG_VERSION && telemetry.length > 0) {
      return invalid(`log version ${EVENT_LOG_VERSION} must not contain telemetry events`);
    }
    for (let i = 0; i < events.length; i++) {
      if (events[i].seq !== i + 1)
        return invalid(`seq gap or duplicate at index ${i}: expected ${i + 1}, got ${events[i].seq}`);
      if (i > 0 && events[i].t < events[i - 1].t)
        return invalid(`time went backwards at seq ${events[i].seq}`);
    }
    if (events.length > 0 && events[0].t < 0) return invalid("first event has negative t");
    if (telemetry.length > 0) {
      const held = /* @__PURE__ */ new Map();
      let unpaired = 0;
      for (const e of telemetry) {
        if (e.kind === "down") {
          held.set(e.code, ((_a = held.get(e.code)) != null ? _a : 0) + 1);
        } else {
          const open = (_b = held.get(e.code)) != null ? _b : 0;
          if (open > 0) held.set(e.code, open - 1);
          else unpaired++;
        }
      }
      if (unpaired > 0) {
        flags.push({
          code: "unpaired-keyup",
          score: Math.min(1, unpaired / telemetry.length),
          detail: `${unpaired} key release(s) without a preceding press`
        });
      }
    }
    if (config.nospace && events.some((e) => e.kind === "commit")) {
      return invalid(
        "nospace log must contain no commit events (progression is derived from inserts)"
      );
    }
    const startT = config.startPolicy === "go" ? asMs(0) : (_d = (_c = stateEvents[0]) == null ? void 0 : _c.t) != null ? _d : asMs(0);
    const deadline = startT + config.durationMs;
    if (config.mode === "time") {
      const past = stateEvents.find((e) => e.t >= deadline);
      if (past)
        return invalid(`event at seq ${past.seq} (t=${past.t}) is at/after the deadline ${deadline}`);
    }
    const endMs = config.mode === "time" ? asMs(deadline) : void 0;
    const folded = foldLog(ctx, events, endMs);
    if (folded.isErr()) {
      return invalid(`replay rejected event seq ${folded.error.at}: ${folded.error.error.kind}`);
    }
    let finalState = folded.value;
    if (config.minWpm > 0 && finalState.phase === "running") {
      const failAt = minSpeedFailInstant(ctx, finalState);
      if (failAt !== null) finalState = settle(ctx, finalState, failAt);
    }
    const metrics = computeMetrics(ctx, events, (_f = (_e = finalState.finishedAt) != null ? _e : endMs) != null ? _f : startT);
    const multiGrapheme = events.filter(
      (e) => e.kind === "insert" && graphemeCount(e.text) > 1
    ).length;
    if (multiGrapheme > 0) {
      flags.push({
        code: "multi-grapheme-insert",
        score: Math.min(1, multiGrapheme / Math.max(1, events.length)),
        detail: `${multiGrapheme} insert event(s) carried more than one grapheme`
      });
    }
    const pastes = events.filter((e) => e.kind === "replace" && e.source === "paste").length;
    if (pastes > 0) {
      flags.push({
        code: "paste",
        score: Math.min(1, pastes / Math.max(1, events.length)),
        detail: `${pastes} paste event(s)`
      });
    }
    const insertTimes = events.filter((e) => e.kind === "insert").map((e) => e.t);
    const intervals = [];
    for (let i = 1; i < insertTimes.length; i++) intervals.push(insertTimes[i] - insertTimes[i - 1]);
    if (intervals.length >= 2) {
      const tooFast = intervals.filter((d) => d < thresholds.minKeyIntervalMs).length;
      if (tooFast > 0) {
        flags.push({
          code: "min-interval",
          score: tooFast / intervals.length,
          detail: `${tooFast}/${intervals.length} intervals < ${thresholds.minKeyIntervalMs}ms`
        });
      }
      const mean = intervals.reduce((sum, d) => sum + d, 0) / intervals.length;
      const variance = intervals.reduce((sum, d) => sum + (d - mean) ** 2, 0) / intervals.length;
      const uniform = intervals.filter(
        (d) => Math.abs(d - mean) <= thresholds.uniformToleranceMs
      ).length;
      const uniformRatio = uniform / intervals.length;
      if (uniformRatio >= thresholds.uniformFlagRatio) {
        flags.push({
          code: "uniform-intervals",
          score: uniformRatio,
          detail: `${Math.round(uniformRatio * 100)}% of intervals within \xB1${thresholds.uniformToleranceMs}ms of the mean`
        });
      }
      if (variance === 0) {
        flags.push({ code: "zero-variance", score: 1, detail: "all keystroke intervals identical" });
      }
    }
    if (metrics.wpm > thresholds.maxBurstWpm && metrics.accuracy === 1) {
      flags.push({
        code: "superhuman-burst",
        score: Math.min(1, metrics.wpm / (thresholds.maxBurstWpm * 2)),
        detail: `${Math.round(metrics.wpm)} wpm at 100% accuracy`
      });
    }
    const runEnd = (_h = (_g = finalState.finishedAt) != null ? _g : endMs) != null ? _h : stateEvents.length > 0 ? stateEvents[stateEvents.length - 1].t : startT;
    const afk = afkBetween(events, finalState.startedAt, runEnd);
    const runMs = Math.max(0, runEnd - ((_i = finalState.startedAt) != null ? _i : startT));
    if (afk.afkMs > 0 && runMs > 0) {
      const share = afk.afkMs / runMs;
      if (share >= thresholds.afkFlagShare) {
        flags.push({
          code: "afk-heavy",
          score: Math.min(1, share),
          detail: `${afk.buckets}s of ${Math.round(runMs / 1e3)}s idle (${Math.round(share * 100)}%)`
        });
      }
    }
    const lastEventT = stateEvents.length > 0 ? stateEvents[stateEvents.length - 1].t : null;
    if (lastEventT !== null) {
      const tailMs = runEnd - lastEventT;
      if (tailMs >= thresholds.trailingAfkMs) {
        flags.push({
          code: "trailing-afk",
          score: runMs > 0 ? Math.min(1, tailMs / runMs) : 1,
          detail: `${Math.round(tailMs / 1e3)}s idle after the last keystroke`
        });
      }
    }
    return ok({ verdict: "valid", flags, metrics });
  }

  // src/parse.ts
  var isRecord = (value) => typeof value === "object" && value !== null && !Array.isArray(value);
  var isValidSeq = (value) => typeof value === "number" && Number.isInteger(value) && value >= 1;
  var isValidT = (value) => typeof value === "number" && Number.isFinite(value) && value >= 0;
  var isDeleteUnit = (value) => value === "char" || value === "word";
  var isReplaceSource = (value) => value === "ime" || value === "paste";
  var isKeyCode = (value) => typeof value === "string" && /^[A-Za-z0-9]{1,32}$/.test(value);
  var isLogVersion = (value) => value === EVENT_LOG_VERSION || value === EVENT_LOG_VERSION_TELEMETRY;
  var isIndex = (value) => typeof value === "number" && Number.isInteger(value) && value >= 0;
  function parseGameEvent(input, version = EVENT_LOG_VERSION) {
    if (!isRecord(input)) {
      return err({
        code: "bad-shape",
        message: `event must be an object, got ${input === null ? "null" : typeof input}`
      });
    }
    const { seq, t, kind } = input;
    if (!isValidSeq(seq)) {
      return err({
        code: "bad-seq",
        message: `seq must be an integer >= 1, got ${JSON.stringify(seq)}`
      });
    }
    if (!isValidT(t)) {
      return err({
        code: "bad-t",
        message: `t must be a finite number >= 0, got ${JSON.stringify(t)}`
      });
    }
    switch (kind) {
      case "insert": {
        const { text } = input;
        if (typeof text !== "string" || text.length === 0) {
          return err({ code: "bad-shape", message: "insert.text must be a non-empty string" });
        }
        return ok(insertEvent(seq, t, text));
      }
      case "delete": {
        const { unit } = input;
        if (!isDeleteUnit(unit)) {
          return err({
            code: "bad-shape",
            message: `delete.unit must be 'char' | 'word', got ${JSON.stringify(unit)}`
          });
        }
        return ok(deleteEvent(seq, t, unit));
      }
      case "commit":
        return ok(commitEvent(seq, t));
      case "replace": {
        const { from, to, text, source } = input;
        if (!isIndex(from) || !isIndex(to) || to < from) {
          return err({
            code: "bad-shape",
            message: `replace range must be integers 0 <= from <= to, got [${JSON.stringify(from)},${JSON.stringify(to)})`
          });
        }
        if (typeof text !== "string") {
          return err({ code: "bad-shape", message: "replace.text must be a string" });
        }
        if (!isReplaceSource(source)) {
          return err({
            code: "bad-shape",
            message: `replace.source must be 'ime' | 'paste', got ${JSON.stringify(source)}`
          });
        }
        return ok(replaceEvent(seq, t, from, to, text, source));
      }
      case "down":
      case "up": {
        if (version !== EVENT_LOG_VERSION_TELEMETRY) {
          return err({ code: "bad-kind", message: `unknown event kind ${JSON.stringify(kind)}` });
        }
        const { code } = input;
        if (!isKeyCode(code)) {
          return err({
            code: "bad-shape",
            message: `${kind}.code must be 1-32 chars of the KeyboardEvent.code charset, got ${JSON.stringify(code)}`
          });
        }
        return ok(kind === "down" ? keyDownEvent(seq, t, code) : keyUpEvent(seq, t, code));
      }
      default:
        return err({ code: "bad-kind", message: `unknown event kind ${JSON.stringify(kind)}` });
    }
  }
  function parseEventBatch(input) {
    if (!isRecord(input)) {
      return err({
        code: "bad-shape",
        message: `batch must be an object, got ${input === null ? "null" : typeof input}`
      });
    }
    if (!isLogVersion(input.version)) {
      return err({
        code: "bad-version",
        message: `unsupported log version ${JSON.stringify(input.version)}, expected ${EVENT_LOG_VERSION} or ${EVENT_LOG_VERSION_TELEMETRY}`
      });
    }
    if (!Array.isArray(input.events)) {
      return err({ code: "bad-shape", message: "batch.events must be an array" });
    }
    const events = [];
    for (let i = 0; i < input.events.length; i++) {
      const parsed = parseGameEvent(input.events[i], input.version);
      if (parsed.isErr()) {
        return err(__spreadProps(__spreadValues({}, parsed.error), { index: i, message: `events[${i}]: ${parsed.error.message}` }));
      }
      events.push(parsed.value);
    }
    return ok({ version: input.version, events });
  }
  return __toCommonJS(index_exports);
})();
//# typemore-core-build {"version":"2.0.0","eventLogVersion":1,"telemetryLogVersion":2,"gitSha":"ec30c943aa940accc65ec9aa0716d9db380f08a0","gitDirty":false}
