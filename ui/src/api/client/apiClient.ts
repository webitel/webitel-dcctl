import axios from 'axios';

import { API_BASE } from './apiBase';

export const apiClient = axios.create({ baseURL: API_BASE });
