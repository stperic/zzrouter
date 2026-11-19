// This parses a transport document for review. The enrollment API validates
// expiry, identity and trust before pairing; browser checks grant no authority.
export function parsePairingTransfer(value) {
  if (new TextEncoder().encode(value).length > 8192) throw new Error('The pairing file must be 8 KiB or smaller.');
  let data;
  try { data = JSON.parse(value); } catch { throw new Error('Paste the JSON pairing details printed by the new computer.'); }
  if (!data || Array.isArray(data) || typeof data !== 'object' || data.type !== 'zzrouter-pairing' || data.version !== 1) throw new Error('This is not a supported zzRouter pairing file.');
  const allowed = ['type', 'version', 'node_name', 'code', 'worker_url', 'worker_fingerprint', 'expires_at'];
  if (Object.keys(data).some(key => !allowed.includes(key)) || allowed.slice(2).some(key => typeof data[key] !== 'string' || !data[key])) throw new Error('The pairing details are incomplete or contain unsupported fields.');
  return data;
}
