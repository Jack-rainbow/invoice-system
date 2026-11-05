import { createDeviceKey, importPublicJwk, jwkThumbprint, signBytes, verifyBytes } from './keys.ts';
import type { DeviceKey, PublicDeviceJwk } from './keys.ts';

type KeyRecord = { id: string; privateKey: CryptoKey; publicJwk: PublicDeviceJwk; thumbprint: string };
type BindingRecord = { id: string; keyId: string; thumbprint: string };
export type CandidateDeviceKey = Readonly<{ id: string; key: DeviceKey }>;

export class DeviceKeyStorageError extends Error {
  readonly code: 'DEVICE_KEY_UNAVAILABLE' | 'DEVICE_KEY_STORAGE_UNAVAILABLE';
  constructor(code: DeviceKeyStorageError['code'], message: string, options?: ErrorOptions) {
    super(message, options); this.name = 'DeviceKeyStorageError'; this.code = code;
  }
}

function unavailable(cause?: unknown): DeviceKeyStorageError {
  return new DeviceKeyStorageError('DEVICE_KEY_UNAVAILABLE', '原设备密钥不可用；停止持钥认证并联系管理员指定公钥交接', { cause });
}

function readRecord<T>(db: IDBDatabase, storeName: string, id: string): Promise<T | undefined> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(storeName, 'readonly'); const request = tx.objectStore(storeName).get(id);
    tx.oncomplete = () => resolve(request.result as T | undefined);
    tx.onerror = () => reject(tx.error); tx.onabort = () => reject(tx.error);
  });
}

async function checkKey(record: KeyRecord | undefined): Promise<DeviceKey> {
  if (!record) throw unavailable();
  try {
    const publicKey = await importPublicJwk(record.publicJwk);
    const thumbprint = await jwkThumbprint(record.publicJwk);
    if (thumbprint !== record.thumbprint) throw unavailable();
    const selfTest = crypto.getRandomValues(new Uint8Array(32));
    if (!(await verifyBytes(publicKey, selfTest, await signBytes(record.privateKey, selfTest)))) throw unavailable();
    return Object.freeze({ privateKey: record.privateKey, publicKey, publicJwk: Object.freeze({ ...record.publicJwk }), thumbprint });
  } catch (cause) { throw unavailable(cause); }
}

export class DeviceKeyStore {
  #db: IDBDatabase;
  constructor(db: IDBDatabase) { this.#db = db; }
  close(): void { this.#db.close(); }

  /** Explicit new candidate only. Never called implicitly by a binding lookup. */
  async createCandidateDeviceKey(): Promise<CandidateDeviceKey> {
    const key = await createDeviceKey(); const id = crypto.randomUUID();
    const record: KeyRecord = { id, privateKey: key.privateKey, publicJwk: key.publicJwk, thumbprint: key.thumbprint };
    try {
      await new Promise<void>((resolve, reject) => {
        const tx = this.#db.transaction('keys', 'readwrite', { durability: 'strict' });
        tx.oncomplete = () => resolve(); tx.onerror = () => reject(tx.error); tx.onabort = () => reject(tx.error);
        // CryptoKey structured clone. No private JWK, strings, localStorage or fallback.
        tx.objectStore('keys').add(record);
      });
      // Do not offer the candidate for setup/pairing until a persisted read signs.
      return Object.freeze({ id, key: await this.requireExistingDeviceKey(id) });
    } catch (cause) {
      throw new DeviceKeyStorageError('DEVICE_KEY_STORAGE_UNAVAILABLE', '设备密钥写入或重读失败，已停止初始化', { cause });
    }
  }

  async requireExistingDeviceKey(id: string): Promise<DeviceKey> {
    if (!id) throw unavailable();
    try { return await checkKey(await readRecord<KeyRecord>(this.#db, 'keys', id)); }
    catch (cause) { throw unavailable(cause); }
  }

  /** Recover an already persisted candidate after a lost bind response. The
   * public server thumbprint is only a lookup; a successful signature is still
   * required before rememberBinding. Never create or export a replacement key. */
  async requireExistingKeyByThumbprint(thumbprint: string): Promise<CandidateDeviceKey> {
    try {
      if (!/^[A-Za-z0-9_-]{43}$/.test(thumbprint)) throw unavailable();
      const records = await new Promise<KeyRecord[]>((resolve, reject) => {
        const tx = this.#db.transaction('keys', 'readonly'); const req = tx.objectStore('keys').getAll();
        tx.oncomplete = () => resolve(req.result as KeyRecord[]); tx.onerror = () => reject(tx.error); tx.onabort = () => reject(tx.error);
      });
      const record = records.find(value => value.thumbprint === thumbprint);
      if (!record) throw unavailable();
      return Object.freeze({ id: record.id, key: await this.requireExistingDeviceKey(record.id) });
    } catch (cause) { throw unavailable(cause); }
  }

  /** Only after the server has confirmed this exact public key's binding. */
  async rememberBinding(bindingId: string, keyId: string, confirmedThumbprint: string): Promise<void> {
    if (!bindingId || !keyId || !confirmedThumbprint) throw unavailable();
    const key = await this.requireExistingDeviceKey(keyId);
    if (key.thumbprint !== confirmedThumbprint) throw unavailable();
    await new Promise<void>((resolve, reject) => {
      const tx = this.#db.transaction(['keys', 'bindings'], 'readwrite', { durability: 'strict' });
      let error: unknown;
      tx.oncomplete = () => resolve(); tx.onerror = () => reject(error ?? tx.error); tx.onabort = () => reject(error ?? tx.error);
      const existing = tx.objectStore('bindings').get(bindingId);
      const stored = tx.objectStore('keys').get(keyId);
      let completed = 0;
      const checked = () => {
        if (++completed !== 2) return;
        const previous = existing.result as BindingRecord | undefined;
        const current = stored.result as KeyRecord | undefined;
        if (!current || current.thumbprint !== confirmedThumbprint ||
            (previous && (previous.keyId !== keyId || previous.thumbprint !== confirmedThumbprint))) {
          error = unavailable(); tx.abort(); return;
        }
        if (!previous) tx.objectStore('bindings').add({ id: bindingId, keyId, thumbprint: confirmedThumbprint } satisfies BindingRecord);
      };
      existing.onsuccess = checked; stored.onsuccess = checked;
    });
  }

  /** Missing record or erased site data always requires recovery, never new keys. */
  async requireBoundDeviceKey(bindingId: string): Promise<DeviceKey> {
    try {
      const binding = await readRecord<BindingRecord>(this.#db, 'bindings', bindingId);
      if (!binding) throw unavailable();
      const key = await this.requireExistingDeviceKey(binding.keyId);
      if (key.thumbprint !== binding.thumbprint) throw unavailable();
      return key;
    } catch (cause) { throw unavailable(cause); }
  }
}

export async function openDeviceKeyStore(options: { databaseName?: string } = {}): Promise<DeviceKeyStore> {
  if (typeof indexedDB === 'undefined') {
    throw new DeviceKeyStorageError('DEVICE_KEY_STORAGE_UNAVAILABLE', 'IndexedDB 不可用，无法安全保存设备私钥');
  }
  const databaseName = options.databaseName ?? 'invoice-device-v1';
  return new Promise((resolve, reject) => {
    let request: IDBOpenDBRequest;
    try { request = indexedDB.open(databaseName, 1); }
    catch (cause) {
      reject(new DeviceKeyStorageError('DEVICE_KEY_STORAGE_UNAVAILABLE', '当前浏览器拒绝 IndexedDB 访问', { cause })); return;
    }
    let failed = false;
    const fail = (cause?: unknown) => {
      failed = true;
      reject(new DeviceKeyStorageError('DEVICE_KEY_STORAGE_UNAVAILABLE', 'IndexedDB 打开失败或被旧标签页阻塞', { cause }));
    };
    request.onblocked = () => fail(); request.onerror = () => fail(request.error);
    request.onupgradeneeded = () => {
      request.result.createObjectStore('keys', { keyPath: 'id' });
      request.result.createObjectStore('bindings', { keyPath: 'id' });
    };
    request.onsuccess = () => {
      const db = request.result;
      if (failed) { db.close(); return; }
      db.onversionchange = () => db.close();
      resolve(new DeviceKeyStore(db));
    };
  });
}
