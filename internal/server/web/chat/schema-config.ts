import { config } from 'zod/v4/core';

// Configure before SDK modules construct schemas and probe dynamic evaluation.
config({ jitless: true });
